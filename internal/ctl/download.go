package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Downloader fetches files over HTTPS.
//
// This replaces `sshpass -p <password> scp -o StrictHostKeyChecking=no`.
// Disabling host key checking removed the only authentication of the remote
// server, and one of the fetched artifacts is a SQL dump that is subsequently
// executed against the database — so anyone able to spoof the storage host
// could execute arbitrary SQL. HTTPS with the system CA bundle authenticates
// the server and needs no credentials, since every file is public.
type Downloader struct {
	Client    *http.Client
	UserAgent string
}

// NewDownloader returns a Downloader with timeouts appropriate for multi-GB
// files on a slow link: no overall deadline, but a bounded handshake and a
// response-header timeout so a black-holed connection cannot hang forever.
func NewDownloader(userAgent string) *Downloader {
	return &Downloader{
		UserAgent: userAgent,
		Client: &http.Client{
			Transport: &http.Transport{
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// Fetch downloads url to dest, resuming a partial file when the server supports
// it. When sha256Hex is non-empty the completed file is verified and removed on
// mismatch.
func (d *Downloader) Fetch(ctx context.Context, url, dest, sha256Hex string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	var offset int64
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		offset = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// nominatim.org rejects requests without a User-Agent.
	req.Header.Set("User-Agent", d.UserAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusOK:
		flags |= os.O_TRUNC // server ignored the range; start over
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		// Already complete.
		return verifyChecksum(dest, sha256Hex)
	default:
		return fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}

	f, err := os.OpenFile(dest, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return verifyChecksum(dest, sha256Hex)
}

func verifyChecksum(path, want string) error {
	if want == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		os.Remove(path)
		return fmt.Errorf("checksum mismatch for %s: want %s, got %s", path, want, got)
	}
	return nil
}

// Reachable reports whether url answers a HEAD or ranged GET within timeout.
// Used to decide whether replication can be configured at all.
func (d *Downloader) Reachable(ctx context.Context, url string, attempts int, delay time.Duration) bool {
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(delay):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(cctx, http.MethodHead, url, nil)
		if err != nil {
			cancel()
			return false
		}
		req.Header.Set("User-Agent", d.UserAgent)
		resp, err := d.Client.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 400 {
				return true
			}
		}
		Logf("replication URL not reachable (attempt %d/%d)", i+1, attempts)
	}
	return false
}
