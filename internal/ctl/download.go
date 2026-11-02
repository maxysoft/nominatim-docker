package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const fetchAttempts = 5

type Downloader struct {
	Client    *http.Client
	UserAgent string

	Backoff     time.Duration
	IdleTimeout time.Duration
}

func NewDownloader(userAgent string) *Downloader {
	// Never relax TLS verification: a downloaded SQL dump is executed against the database.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSHandshakeTimeout = 30 * time.Second
	t.ResponseHeaderTimeout = 60 * time.Second
	return &Downloader{
		UserAgent:   userAgent,
		Backoff:     2 * time.Second,
		IdleTimeout: 60 * time.Second,
		Client:      &http.Client{Transport: t},
	}
}

type errPermanent struct{ err error }

func (e errPermanent) Error() string { return e.err.Error() }
func (e errPermanent) Unwrap() error { return e.err }

func (d *Downloader) Fetch(ctx context.Context, url, dest, sha256Hex string) error {
	backoff := d.Backoff

	var last error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		if attempt > 1 {
			Logf("retrying %s (attempt %d/%d) after %v: %v", url, attempt, fetchAttempts, backoff, last)
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
	return fmt.Errorf("downloading %s failed after %d attempts: %w", url, fetchAttempts, last)
}

type fetchState struct {
	URL       string `json:"url"`
	Validator string `json:"validator,omitempty"`
	Complete  bool   `json:"complete"`
}

func statePath(dest string) string { return dest + ".source" }

func readState(dest string) fetchState {
	var st fetchState
	if b, err := os.ReadFile(statePath(dest)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func writeState(dest string, st fetchState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileNoFollow(statePath(dest), b)
}

func writeFileNoFollow(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func strongValidator(h http.Header) string {
	if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		return etag
	}
	return h.Get("Last-Modified")
}

type idleReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.timer.Reset(i.idle)
	}
	return n, err
}

func (d *Downloader) fetchOnce(ctx context.Context, url, dest, sha256Hex string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return errPermanent{err}
	}
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(dest); err != nil {
			return errPermanent{err}
		}
	}

	st := readState(dest)
	fi, statErr := os.Stat(dest)
	if statErr == nil && st.URL == url && st.Complete {
		err := verifyChecksum(dest, sha256Hex)
		if err == nil {
			return nil
		}
		Logf("%v; downloading again", err)
		st, statErr = fetchState{}, os.ErrNotExist
	}
	var offset int64
	if statErr == nil && st.URL == url && st.Validator != "" && fi.Size() > 0 {
		offset = fi.Size()
	}

	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
	if err != nil {
		return errPermanent{err}
	}
	req.Header.Set("User-Agent", d.UserAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		req.Header.Set("If-Range", st.Validator)
	}

	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW
	switch resp.StatusCode {
	case http.StatusOK:
		flags |= os.O_TRUNC
	case http.StatusPartialContent:
		if start, ok := parseContentRangeStart(resp.Header.Get("Content-Range")); !ok || start != offset {
			Logf("server returned an unexpected Content-Range (%q, wanted start %d); restarting download",
				resp.Header.Get("Content-Range"), offset)
			// Removed, not truncated: Truncate would follow a swapped-in symlink.
			if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errPermanent{err}
			}
			return fmt.Errorf("range mismatch for %s", url)
		}
		flags |= os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		if total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok && offset > 0 && total == offset {
			st.Complete = true
			if err := writeState(dest, st); err != nil {
				return errPermanent{err}
			}
			return verifyChecksum(dest, sha256Hex)
		}
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errPermanent{err}
		}
		return fmt.Errorf("GET %s: %s; restarting download", url, resp.Status)
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	default:
		if resp.StatusCode >= 500 {
			return fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		return errPermanent{fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)}
	}

	f, err := os.OpenFile(dest, flags, 0o644)
	if err != nil {
		return errPermanent{err}
	}
	if resp.StatusCode == http.StatusOK {
		st = fetchState{URL: url, Validator: strongValidator(resp.Header)}
		if err := writeState(dest, st); err != nil {
			f.Close()
			return errPermanent{err}
		}
	}
	timer := time.AfterFunc(d.IdleTimeout, cancel)
	defer timer.Stop()
	body := &idleReader{r: resp.Body, timer: timer, idle: d.IdleTimeout}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		if ctx.Err() == nil && actx.Err() != nil {
			return fmt.Errorf("downloading %s: no data received for %v", url, d.IdleTimeout)
		}
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return errPermanent{err}
	}
	st.Complete = true
	if err := writeState(dest, st); err != nil {
		return errPermanent{err}
	}
	return verifyChecksum(dest, sha256Hex)
}

func parseContentRangeStart(h string) (int64, bool) {
	var start int64
	n, err := fmt.Sscanf(h, "bytes %d-", &start)
	return start, err == nil && n == 1
}

func parseContentRangeTotal(h string) (int64, bool) {
	_, total, ok := strings.Cut(h, "/")
	if !ok || !strings.HasPrefix(h, "bytes ") {
		return 0, false
	}
	n, err := strconv.ParseInt(total, 10, 64)
	return n, err == nil
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
		os.Remove(path)
		os.Remove(statePath(path))
		return errPermanent{fmt.Errorf("checksum mismatch for %s: want %s, got %s", path, want, got)}
	}
	return nil
}

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
			Logf("replication URL %s returned %s (attempt %d/%d)", url, resp.Status, i+1, attempts)
			continue
		}
		Logf("replication URL %s unreachable (attempt %d/%d): %v", url, i+1, attempts, err)
	}
	return false
}
