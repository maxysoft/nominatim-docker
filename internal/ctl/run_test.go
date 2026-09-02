package ctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// The foreground updater refuses, before touching the database, when it has
// no URL, when the database is frozen, or when osm2pgsql is missing.
func TestReplicateGuards(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cases := map[string]*Config{
		"REPLICATION_URL": {},
		"FREEZE":          {ReplicationURL: "https://example.invalid/updates", Freeze: true},
		"serve-only":      {ReplicationURL: "https://example.invalid/updates"},
	}
	for want, c := range cases {
		if err := Replicate(context.Background(), c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Replicate = %v, want an error mentioning %q", err, want)
		}
	}
}
