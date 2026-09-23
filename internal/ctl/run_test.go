package ctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	// FREEZE must not silently skip replication the import set up.
	c2 := &Config{ReplicationURL: "https://example.invalid/updates", UpdateMode: "continuous", Freeze: true}
	if _, _, err := startReplication(context.Background(), c2, &Runner{}, false); err == nil || !strings.Contains(err.Error(), "osm2pgsql") {
		t.Fatalf("startReplication = %v, want osm2pgsql refusal", err)
	}

	c3 := &Config{ReplicationURL: "https://example.invalid/updates"}
	if stop, _, err := startReplication(context.Background(), c3, &Runner{}, false); err != nil || stop != nil {
		t.Fatalf("startReplication = (stop set: %v, %v), want clean skip", stop != nil, err)
	}
}

// The foreground updater refuses, before touching the database, when it has
// no URL or when osm2pgsql is missing. FREEZE does not stop it: the import
// lets REPLICATION_URL win, so the database was never frozen.
func TestReplicateGuards(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cases := map[string]*Config{
		"REPLICATION_URL": {},
		"serve-only":      {ReplicationURL: "https://example.invalid/updates", Freeze: true},
	}
	for want, c := range cases {
		if err := Replicate(context.Background(), c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Replicate = %v, want an error mentioning %q", err, want)
		}
	}
}

// The SIGKILL deadline follows GUNICORN_GRACEFUL_TIMEOUT; a fixed 35s cut a
// longer drain short.
func TestRunnerGraceFollowsConfig(t *testing.T) {
	c := &Config{GunicornGracefulTimeout: 90}
	cmd := (&Runner{Grace: c.ShutdownGrace()}).Command(context.Background(), "true")
	if cmd.WaitDelay != 95*time.Second {
		t.Fatalf("WaitDelay = %v, want 95s", cmd.WaitDelay)
	}
	if got := (&Runner{}).Command(context.Background(), "true").WaitDelay; got != shutdownGrace {
		t.Fatalf("default WaitDelay = %v, want %v", got, shutdownGrace)
	}
}
