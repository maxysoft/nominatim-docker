package ctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testDownloader() *Downloader {
	d := NewDownloader("test-agent")
	d.Backoff = time.Millisecond
	return d
}

func sha256Of(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestFetchWritesFileAndVerifiesChecksum(t *testing.T) {
	body := []byte("hello nominatim")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "test-agent" {
			t.Errorf("missing User-Agent: %q", r.Header.Get("User-Agent"))
		}
		w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(body) {
		t.Fatalf("content = %q", got)
	}
}

func TestFetchChecksumMismatchRemovesFileAndDoesNotRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte("wrong content"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of([]byte("expected")))
	if err == nil {
		t.Fatal("expected a checksum error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("corrupt file was left on disk")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("checksum failure should not be retried, got %d requests", n)
	}
}

func TestFetchRetriesTransientFailure(t *testing.T) {
	body := []byte("0123456789abcdefghij")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("expected 2 requests, got %d", n)
	}
}

func TestFetchDoesNotRetryNotFound(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	err := testDownloader().Fetch(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f"), "")
	if err == nil {
		t.Fatal("expected an error for 404")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("404 should not be retried, got %d requests", n)
	}
}

func TestFetchResumesFromPartialFile(t *testing.T) {
	body := []byte("0123456789abcdefghij")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "f.gz")
	if err := os.WriteFile(dest, body[:8], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dest, fetchState{URL: srv.URL, Validator: `"v1"`}); err != nil {
		t.Fatal(err)
	}
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(body) {
		t.Fatalf("resumed content = %q, want %q", got, body)
	}
}

func TestFetchRestartsOnContentRangeMismatch(t *testing.T) {
	body := []byte("0123456789abcdefghij")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 3-%d/%d", len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body[3:])
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	if err := os.WriteFile(dest, []byte("XXXXXXXX"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dest, fetchState{URL: srv.URL, Validator: `"v1"`}); err != nil {
		t.Fatal(err)
	}
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(body) {
		t.Fatalf("content = %q, want a clean re-download of %q", got, body)
	}
}

func TestFetchHandlesServerIgnoringRange(t *testing.T) {
	body := []byte("0123456789abcdefghij")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	if err := os.WriteFile(dest, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dest, fetchState{URL: srv.URL, Validator: `"v1"`}); err != nil {
		t.Fatal(err)
	}
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(body) {
		t.Fatalf("content = %q, want %q (stale prefix must be truncated)", got, body)
	}
}

func TestFetchDoesNotResumeForeignPartial(t *testing.T) {
	body := []byte("0123456789abcdefghij")
	for name, st := range map[string]func(url string) fetchState{
		"other URL":       func(string) fetchState { return fetchState{URL: "https://example.invalid/other.pbf"} },
		"no record":       func(string) fetchState { return fetchState{} },
		"changed version": func(url string) fetchState { return fetchState{URL: url, Validator: `"v0"`} },
		"no validator":    func(url string) fetchState { return fetchState{URL: url} },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"v1"`)
				http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
			}))
			defer srv.Close()

			dest := filepath.Join(t.TempDir(), "f.gz")
			if err := os.WriteFile(dest, []byte("XXXXXXXX"), 0o644); err != nil {
				t.Fatal(err)
			}
			if s := st(srv.URL); s != (fetchState{}) {
				if err := writeState(dest, s); err != nil {
					t.Fatal(err)
				}
			}
			if err := testDownloader().Fetch(context.Background(), srv.URL, dest, ""); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if got, _ := os.ReadFile(dest); string(got) != string(body) {
				t.Fatalf("content = %q, want a clean download of %q", got, body)
			}
		})
	}
}

func TestFetchRecordsNewStateOnlyAfterTruncating(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte("new version"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	old := fetchState{URL: srv.URL, Validator: `"v0"`}
	if err := writeState(dest, old); err != nil {
		t.Fatal(err)
	}
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, ""); err == nil {
		t.Fatal("Fetch succeeded, want the open to fail")
	}
	if st := readState(dest); st != old {
		t.Fatalf("state = %+v, want the old %+v", st, old)
	}
}

func TestFetchReusesCompletedDownload(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	body := []byte("complete")
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dest, fetchState{URL: srv.URL, Complete: true}); err != nil {
		t.Fatal(err)
	}
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("completed download fetched again (%d requests)", n)
	}
}

func TestFetchReplacesSymlink(t *testing.T) {
	body := []byte("downloaded")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	operator := filepath.Join(dir, "operator.csv.gz")
	if err := os.WriteFile(operator, []byte("operator data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "f.gz")
	if err := os.Symlink(operator, dest); err != nil {
		t.Fatal(err)
	}
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, _ := os.ReadFile(operator); string(got) != "operator data" {
		t.Fatalf("operator file overwritten: %q", got)
	}
	if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("dest is still a symlink (%v)", err)
	}
}

func TestFetchAbandonsStalledBody(t *testing.T) {
	body := []byte("0123456789abcdefghij")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("ETag", `"v1"`)
			w.Write(body[:5])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	d := testDownloader()
	d.IdleTimeout = 100 * time.Millisecond
	dest := filepath.Join(t.TempDir(), "f.gz")
	if err := d.Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("got %d requests, want 2 (stall, then resume)", n)
	}
}

func TestFetchAcceptsFullLengthPartial(t *testing.T) {
	body := []byte("0123456789abcdefghij")
	var full int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			atomic.AddInt32(&full, 1)
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dest, fetchState{URL: srv.URL, Validator: `"v1"`}); err != nil {
		t.Fatal(err)
	}
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n := atomic.LoadInt32(&full); n != 0 {
		t.Fatalf("full-length partial downloaded again (%d full requests)", n)
	}
	if st := readState(dest); !st.Complete {
		t.Fatal("completion not recorded")
	}
}

func TestFetchRefetchesCompletedFileOnChecksumChange(t *testing.T) {
	body := []byte("republished")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.gz")
	if err := os.WriteFile(dest, []byte("old version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dest, fetchState{URL: srv.URL, Complete: true}); err != nil {
		t.Fatal(err)
	}
	if err := testDownloader().Fetch(context.Background(), srv.URL, dest, sha256Of(body)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(body) {
		t.Fatalf("content = %q, want %q", got, body)
	}
}

func TestParseContentRangeStart(t *testing.T) {
	cases := map[string]struct {
		want int64
		ok   bool
	}{
		"bytes 0-99/100":   {0, true},
		"bytes 8-19/20":    {8, true},
		"bytes */20":       {0, false},
		"items 0-99/100":   {0, false},
		"":                 {0, false},
		"bytes abc-99/100": {0, false},
	}
	for in, want := range cases {
		got, ok := parseContentRangeStart(in)
		if ok != want.ok || (ok && got != want.want) {
			t.Errorf("parseContentRangeStart(%q) = (%d,%v), want (%d,%v)", in, got, ok, want.want, want.ok)
		}
	}
}

func TestReachable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer down.Close()

	d := testDownloader()
	if !d.Reachable(context.Background(), up.URL, 2, time.Millisecond) {
		t.Error("reachable server reported unreachable")
	}
	if d.Reachable(context.Background(), down.URL, 2, time.Millisecond) {
		t.Error("404 server reported reachable")
	}
}
