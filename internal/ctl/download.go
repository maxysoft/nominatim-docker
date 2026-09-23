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

// Downloader fetches files over HTTPS, authenticated by the system CA bundle.
// One of the fetched artifacts is a SQL dump that is executed against the
// database, so server authentication is not optional.
type Downloader struct {
	Client    *http.Client
	UserAgent string

	// Attempts is the total number of tries per file: a planet PBF takes
	// hours, and one transient reset must not discard it.
	Attempts int
	// Backoff is the delay before the second attempt, doubled each time.
	Backoff time.Duration
	// IdleTimeout aborts an attempt that receives no body bytes for this
	// long, so a stalled but open connection is retried instead of hanging.
	IdleTimeout time.Duration
}

// NewDownloader returns a Downloader tuned for multi-GB files on a slow link:
// no overall deadline, but bounded handshake, response-header and idle-body
// timeouts so a black-holed or stalled connection cannot hang forever.
func NewDownloader(userAgent string) *Downloader {
	return &Downloader{
		UserAgent:   userAgent,
		Attempts:    5,
		Backoff:     2 * time.Second,
		IdleTimeout: 60 * time.Second,
		Client: &http.Client{
			Transport: &http.Transport{
				// HTTPS_PROXY/HTTP_PROXY/NO_PROXY, as curl honoured them.
				Proxy:                 http.ProxyFromEnvironment,
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

// Fetch downloads url to dest, resuming a partial file when the server
// supports it and retrying transient failures. When sha256Hex is non-empty
// the completed file is verified and removed on mismatch.
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

// fetchState records what dest holds, so a later run resumes only a partial
// download of the same URL and version instead of splicing another file on.
type fetchState struct {
	URL       string `json:"url"`
	Validator string `json:"validator,omitempty"` // strong ETag or Last-Modified
	Complete  bool   `json:"complete"`
}

func statePath(dest string) string { return dest + ".source" }

func readState(dest string) fetchState {
	var st fetchState
	if b, err := os.ReadFile(statePath(dest)); err == nil {
		_ = json.Unmarshal(b, &st) // unreadable state just means "start over"
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

// writeFileNoFollow replaces path with data. The project directory is writable
// by the workload user, so a planted symlink must be replaced, not followed.
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

// strongValidator returns what If-Range may carry: a strong ETag, else
// Last-Modified. A weak ETag is not allowed there.
func strongValidator(h http.Header) string {
	if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		return etag
	}
	return h.Get("Last-Modified")
}

// idleReader pushes back a deadline timer on every read that returns data.
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
	// A symlink left by an earlier IMPORT_X=/path run must be replaced, not
	// written through into the operator's own file.
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(dest); err != nil {
			return errPermanent{err}
		}
	}

	st := readState(dest)
	fi, statErr := os.Stat(dest)
	if statErr == nil && st.URL == url && st.Complete {
		err := verifyChecksum(dest, sha256Hex) // finished by an earlier run
		if err == nil {
			return nil
		}
		// Upstream republished and the operator updated the checksum:
		// verifyChecksum removed the old file, so fetch the current one.
		Logf("%v; downloading again", err)
		st, statErr = fetchState{}, os.ErrNotExist
	}
	// Resume only our own partial of this URL; anything else starts over.
	var offset int64
	if statErr == nil && st.URL == url && fi.Size() > 0 {
		offset = fi.Size()
	}

	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
	if err != nil {
		return errPermanent{err}
	}
	// nominatim.org rejects requests without a User-Agent.
	req.Header.Set("User-Agent", d.UserAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		// Upstream republished the file since: the server sends it whole.
		if st.Validator != "" {
			req.Header.Set("If-Range", st.Validator)
		}
	}

	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err) // transient: retry
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY | syscall.O_NOFOLLOW
	switch resp.StatusCode {
	case http.StatusOK:
		flags |= os.O_TRUNC // new file, or the server ignored the range
		st = fetchState{URL: url, Validator: strongValidator(resp.Header)}
		if err := writeState(dest, st); err != nil {
			return errPermanent{err}
		}
	case http.StatusPartialContent:
		// Trust the range only if it starts where our file ends; a mismatched
		// offset would splice unrelated bytes into the file.
		if start, ok := parseContentRangeStart(resp.Header.Get("Content-Range")); !ok || start != offset {
			Logf("server returned an unexpected Content-Range (%q, wanted start %d); restarting download",
				resp.Header.Get("Content-Range"), offset)
			// Removed, not truncated: Truncate would follow a swapped-in symlink.
			if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errPermanent{err}
			}
			return fmt.Errorf("range mismatch for %s", url) // retry from scratch
		}
		flags |= os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		// A partial of exactly the remote length is a finished download whose
		// completion was never recorded (killed right after the last byte).
		// Only with If-Range sent: that is what confirms the same version.
		if total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok && offset > 0 && st.Validator != "" && total == offset {
			st.Complete = true
			if err := writeState(dest, st); err != nil {
				return errPermanent{err}
			}
			return verifyChecksum(dest, sha256Hex)
		}
		// Longer than the remote file, or unverifiable: start over.
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errPermanent{err}
		}
		return fmt.Errorf("GET %s: %s; restarting download", url, resp.Status)
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
	var body io.Reader = resp.Body
	if d.IdleTimeout > 0 {
		timer := time.AfterFunc(d.IdleTimeout, cancel)
		defer timer.Stop()
		body = &idleReader{r: resp.Body, timer: timer, idle: d.IdleTimeout}
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		if ctx.Err() == nil && actx.Err() != nil {
			return fmt.Errorf("downloading %s: no data received for %v", url, d.IdleTimeout) // resume next time
		}
		return fmt.Errorf("downloading %s: %w", url, err) // transient: resume next time
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

// parseContentRangeTotal extracts TOTAL from "bytes */TOTAL" or
// "bytes START-END/TOTAL".
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
		// Removed so a retry starts clean rather than resuming onto bad bytes.
		os.Remove(path)
		os.Remove(statePath(path))
		return errPermanent{fmt.Errorf("checksum mismatch for %s: want %s, got %s", path, want, got)}
	}
	return nil
}

// Reachable reports whether url answers within the attempt budget; used to
// decide whether replication can be configured at all. Failures are logged so
// an unreachable URL is distinguishable from a typo.
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
		Logf("replication URL %s unreachable (attempt %d/%d): %v", url, i+1, attempts, Redact(err.Error()))
	}
	return false
}
