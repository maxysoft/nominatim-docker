package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

	// Attempts is the total number of tries per file. A planet PBF is ~80 GB
	// and takes hours; a single transient reset should not discard it.
	Attempts int
	// Backoff is the delay before the second attempt, doubled each time.
	Backoff time.Duration
}

// NewDownloader returns a Downloader with timeouts appropriate for multi-GB
// files on a slow link: no overall deadline, but a bounded handshake and a
// response-header timeout so a black-holed connection cannot hang forever.
func NewDownloader(userAgent string) *Downloader {
	return &Downloader{
		UserAgent: userAgent,
		Attempts:  5,
		Backoff:   2 * time.Second,
		Client: &http.Client{
			Transport: &http.Transport{
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// errPermanent wraps a failure that retrying cannot fix.
type errPermanent struct{ err error }

func (e errPermanent) Error() string { return e.err.Error() }
func (e errPermanent) Unwrap() error { return e.err }

// Fetch downloads url to dest, resuming a partial file when the server supports
// it and retrying transient failures. When sha256Hex is non-empty the completed
// file is verified and removed on mismatch.
func (d *Downloader) Fetch(ctx context.Context, url, dest, sha256Hex string) error {
	attempts := d.Attempts
	if attempts < 1 {
		attempts = 1
	}
	backoff := d.Backoff

	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			Logf("retrying %s (attempt %d/%d) after %v: %v", url, attempt, attempts, backoff, Redact(last.Error()))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		err := d.fetchOnce(ctx, url, dest, sha256Hex)
		if err == nil {
			return nil
		}
		var perm errPermanent
		if errors.As(err, &perm) || ctx.Err() != nil {
			return err
		}
		last = err
	}
	return fmt.Errorf("downloading %s failed after %d attempts: %w", url, attempts, last)
}

func (d *Downloader) fetchOnce(ctx context.Context, url, dest, sha256Hex string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return errPermanent{err}
	}

	var offset int64
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		offset = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errPermanent{err}
	}
	// nominatim.org rejects requests without a User-Agent.
	req.Header.Set("User-Agent", d.UserAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err) // transient: retry
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusOK:
		flags |= os.O_TRUNC // server ignored the range; start over
	case http.StatusPartialContent:
		// Trust the range only if the server says it starts where we do. A
		// mismatched offset would splice unrelated bytes into the file — for
		// example when a partial download from a previous, different URL is
		// still lying in the project directory.
		if start, ok := parseContentRangeStart(resp.Header.Get("Content-Range")); !ok || start != offset {
			Logf("server returned an unexpected Content-Range (%q, wanted start %d); restarting download",
				resp.Header.Get("Content-Range"), offset)
			if err := os.Truncate(dest, 0); err != nil {
				return errPermanent{err}
			}
			return fmt.Errorf("range mismatch for %s", url) // retry from scratch
		}
		flags |= os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		return verifyChecksum(dest, sha256Hex) // already complete
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	default:
		if resp.StatusCode >= 500 {
			return fmt.Errorf("GET %s: %s", url, resp.Status) // transient
		}
		return errPermanent{fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)}
	}

	f, err := os.OpenFile(dest, flags, 0o644)
	if err != nil {
		return errPermanent{err}
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("downloading %s: %w", url, err) // transient: resume next time
	}
	if err := f.Close(); err != nil {
		return errPermanent{err}
	}
	return verifyChecksum(dest, sha256Hex)
}

// parseContentRangeStart extracts START from "bytes START-END/TOTAL".
func parseContentRangeStart(h string) (int64, bool) {
	const prefix = "bytes "
	if !strings.HasPrefix(h, prefix) {
		return 0, false
	}
	spec, _, ok := strings.Cut(strings.TrimPrefix(h, prefix), "/")
	if !ok {
		return 0, false
	}
	startStr, _, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, false
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return start, true
}

func verifyChecksum(path, want string) error {
	if want == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return errPermanent{err}
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return errPermanent{err}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		// Removed so a retry starts clean rather than resuming onto bad bytes.
		os.Remove(path)
		return errPermanent{fmt.Errorf("checksum mismatch for %s: want %s, got %s", path, want, got)}
	}
	return nil
}

// Reachable reports whether url answers within timeout. Used to decide whether
// replication can be configured at all.
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
			// The reason is logged: the shell version discarded it, so an
			// unreachable replication URL was indistinguishable from a typo.
			Logf("replication URL %s returned %s (attempt %d/%d)", url, resp.Status, i+1, attempts)
			continue
		}
		Logf("replication URL %s unreachable (attempt %d/%d): %v", url, i+1, attempts, Redact(err.Error()))
	}
	return false
}
