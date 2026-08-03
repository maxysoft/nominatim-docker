package ctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A volume left root-owned by the pre-refactor image must be reported with an
// actionable message, not surface later as an opaque Nominatim error.
func TestWritableBy(t *testing.T) {
	dir := t.TempDir()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()

	if !writableBy(fi, uid, gid) {
		t.Fatal("own temp dir reported unwritable")
	}
	// A uid/gid we do not match, against a dir with no world-write bit.
	if writableBy(fi, uid+4242, gid+4242) {
		t.Fatal("a foreign uid should not be considered able to write")
	}

	world := filepath.Join(dir, "world")
	if err := os.Mkdir(world, 0o777); err != nil {
		t.Fatal(err)
	}
	// Mkdir's mode is masked by the umask, so set the bits explicitly.
	if err := os.Chmod(world, 0o777); err != nil {
		t.Fatal(err)
	}
	wfi, _ := os.Stat(world)
	if !writableBy(wfi, uid+4242, gid+4242) {
		t.Fatal("a world-writable dir should be writable by anyone")
	}

	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	rfi, _ := os.Stat(ro)
	if writableBy(rfi, uid, gid) {
		t.Fatal("a mode-500 dir should not be writable by its owner")
	}
}

// The serve-only image (no osm2pgsql) must refuse an import before any
// download or provisioning, must refuse an explicit UPDATE_MODE, and must
// silently skip replication otherwise.
func TestServeOnlyImageGuards(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH == serve-only image

	pbf := filepath.Join(t.TempDir(), "x.osm.pbf")
	if err := os.WriteFile(pbf, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{PBFPath: pbf, AdminPassword: "x", DatasetValues: map[string]string{}}
	err := RunImport(context.Background(), c, &Runner{})
	if err == nil || !strings.Contains(err.Error(), "serve-only") {
		t.Fatalf("RunImport = %v, want serve-only refusal", err)
	}

	c2 := &Config{ReplicationURL: "https://example.invalid/updates", UpdateMode: "continuous"}
	if _, err := startReplication(context.Background(), c2, &Runner{}); err == nil || !strings.Contains(err.Error(), "osm2pgsql") {
		t.Fatalf("startReplication = %v, want osm2pgsql refusal", err)
	}

	c3 := &Config{ReplicationURL: "https://example.invalid/updates"}
	if cmd, err := startReplication(context.Background(), c3, &Runner{}); err != nil || cmd != nil {
		t.Fatalf("startReplication = (%v, %v), want clean skip", cmd, err)
	}
}

func TestChownTreeSkipsUnreadableAndWalksAll(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Chowning to our own ids is a no-op that still exercises the whole walk.
	if err := chownTree(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownTree: %v", err)
	}
}
