package ctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// IMPORT_X=true over a file the operator placed at dest must neither truncate
// it nor, on a checksum mismatch, delete it.
func TestFetchDatasetsRefusesDownloadOverOperatorFile(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte("downloaded"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, Datasets[0].Local)
	if err := os.WriteFile(dest, []byte("operator"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Config{ProjectDir: dir, MirrorBaseURL: srv.URL, DatasetValues: map[string]string{Datasets[0].EnvVar: "true"}}
	if err := fetchDatasets(context.Background(), c, testDownloader()); err == nil {
		t.Fatal("fetchDatasets succeeded, want a refusal")
	}
	if got, _ := os.ReadFile(dest); string(got) != "operator" {
		t.Fatalf("operator file content = %q", got)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("%d requests made, want none", n)
	}
}

// A dataset the operator placed where Nominatim looks for it is used in
// place: deleting it and linking dest to itself made the import skip it. After
// the import, only downloads and symlinks are removed.
func TestDatasetsKeepOperatorFileInProjectDir(t *testing.T) {
	dir := t.TempDir()
	wiki := filepath.Join(dir, Datasets[0].Local)
	if err := os.WriteFile(wiki, []byte("operator"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "us.csv.gz")
	if err := os.WriteFile(target, []byte("us"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Config{ProjectDir: dir, DatasetValues: map[string]string{
		Datasets[0].EnvVar: wiki,
		Datasets[3].EnvVar: target,
	}}
	if err := fetchDatasets(context.Background(), c, nil); err != nil {
		t.Fatalf("fetchDatasets: %v", err)
	}

	gb := filepath.Join(dir, Datasets[2].Local)
	if err := os.WriteFile(gb, []byte("downloaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(gb, fetchState{URL: "https://example.invalid/gb", Complete: true}); err != nil {
		t.Fatal(err)
	}
	cleanupDownloads(c)

	if fi, err := os.Lstat(wiki); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("operator file gone or replaced (%v)", err)
	}
	if got, _ := os.ReadFile(wiki); string(got) != "operator" {
		t.Fatalf("operator file content = %q", got)
	}
	for _, p := range []string{filepath.Join(dir, Datasets[3].Local), gb, statePath(gb)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind (%v)", p, err)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target removed: %v", err)
	}
}

// With a dataset unset, what an earlier failed run left must go before the
// import, or Nominatim imports it anyway; a file of the operator's stays.
func TestFetchDatasetsRemovesOnlyOwnLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := func(i int) string { return filepath.Join(dir, Datasets[i].Local) }
	if err := os.WriteFile(path(0), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(path(0), fetchState{URL: "https://example.invalid/wiki"}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "secondary.sql.gz")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path(1)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(2), []byte("operator"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Config{ProjectDir: dir, DatasetValues: map[string]string{}}
	if err := fetchDatasets(context.Background(), c, nil); err != nil {
		t.Fatalf("fetchDatasets: %v", err)
	}
	for i, want := range []bool{false, false, true} {
		if _, err := os.Lstat(path(i)); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", Datasets[i].Local, err == nil, want)
		}
	}
	if _, err := os.Lstat(statePath(path(0))); !os.IsNotExist(err) {
		t.Errorf("state file left behind (%v)", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target removed: %v", err)
	}
}
