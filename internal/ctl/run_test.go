package ctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeOnlyImageGuards(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	pbf := filepath.Join(t.TempDir(), "x.osm.pbf")
	if err := os.WriteFile(pbf, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{PBFPath: pbf, AdminPassword: "x", DatasetValues: map[string]string{}}
	err := RunImport(context.Background(), c, &Runner{})
	if err == nil || !strings.Contains(err.Error(), "serve-only") {
		t.Fatalf("RunImport = %v, want serve-only refusal", err)
	}

	c2 := &Config{ReplicationURL: "https://example.invalid/updates", UpdateMode: "continuous", Freeze: true}
	if _, _, err := startReplication(context.Background(), c2, &Runner{}, false); err == nil || !strings.Contains(err.Error(), "osm2pgsql") {
		t.Fatalf("startReplication = %v, want osm2pgsql refusal", err)
	}

	c3 := &Config{ReplicationURL: "https://example.invalid/updates"}
	if stop, _, err := startReplication(context.Background(), c3, &Runner{}, false); err != nil || stop != nil {
		t.Fatalf("startReplication = (stop set: %v, %v), want clean skip", stop != nil, err)
	}
}

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

func TestRunnerGraceFollowsConfig(t *testing.T) {
	c := &Config{GunicornGracefulTimeout: 90}
	cmd := (&Runner{Grace: c.ShutdownGrace()}).Command(context.Background(), "true")
	if cmd.WaitDelay != 95*time.Second {
		t.Fatalf("WaitDelay = %v, want 95s", cmd.WaitDelay)
	}
}
