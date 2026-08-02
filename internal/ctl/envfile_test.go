package ctl

import (
	"strings"
	"testing"
)

func testConfig() *Config {
	return &Config{
		ProjectDir:                 "/nominatim",
		PostgresHost:               "db",
		PostgresPort:               5432,
		PostgresDB:                 "nominatim",
		PostgresSSLMode:            "require",
		NominatimPassword:          "pw1",
		WebUser:                    "www-data",
		ImportStyle:                "full",
		ReplicationUpdateInterval:  86400,
		ReplicationRecheckInterval: 900,
		DatasetValues:              map[string]string{},
	}
}

// The old sed-based templating consumed its __PLACEHOLDER__ tokens on the first
// run, so on a persisted volume every later start silently ignored the
// environment. Regenerating must be stable and must track configuration change.
func TestRenderEnvFileIsIdempotent(t *testing.T) {
	c := testConfig()
	first := RenderEnvFile(c)
	second := RenderEnvFile(c)
	if first != second {
		t.Fatalf("render is not stable:\n%s\n---\n%s", first, second)
	}
}

func TestRenderEnvFileTracksConfigChanges(t *testing.T) {
	c := testConfig()
	before := RenderEnvFile(c)

	c.PostgresHost = "new-db"
	c.NominatimPassword = "rotated"
	c.ImportStyle = "admin"
	after := RenderEnvFile(c)

	if before == after {
		t.Fatal("changing host, password and import style produced an identical file")
	}
	for _, want := range []string{"host=new-db", "password=rotated", "NOMINATIM_IMPORT_STYLE=admin"} {
		if !strings.Contains(after, want) {
			t.Fatalf("rendered file missing %q:\n%s", want, after)
		}
	}
	if strings.Contains(after, "host=db;") || strings.Contains(after, "password=pw1") {
		t.Fatalf("stale values survived the re-render:\n%s", after)
	}
}

// The shell version appended this line with >> on every start, growing the file
// without bound on a restart loop.
func TestTigerLineAppearsAtMostOnce(t *testing.T) {
	c := testConfig()
	c.DatasetValues["IMPORT_TIGER_ADDRESSES"] = "true"
	out := RenderEnvFile(c)
	if n := strings.Count(out, "NOMINATIM_USE_US_TIGER_DATA"); n != 1 {
		t.Fatalf("Tiger line appears %d times, want 1:\n%s", n, out)
	}

	c.DatasetValues["IMPORT_TIGER_ADDRESSES"] = ""
	if strings.Contains(RenderEnvFile(c), "NOMINATIM_USE_US_TIGER_DATA") {
		t.Fatal("Tiger line present while Tiger is disabled")
	}
}

// An unset replication URL previously left the literal __REPLICATION_URL__
// placeholder in the live configuration.
func TestNoPlaceholdersLeak(t *testing.T) {
	out := RenderEnvFile(testConfig())
	if strings.Contains(out, "__") {
		t.Fatalf("placeholder token survived rendering:\n%s", out)
	}
	if !strings.Contains(out, "NOMINATIM_REPLICATION_URL=\n") {
		t.Fatalf("empty replication URL should render as an empty value:\n%s", out)
	}
}

// A second render with a different interval must fully replace the first, not
// splice into it the way the unanchored sed did (86400 -> 864000 -> "3000").
func TestIntervalIsReplacedNotSpliced(t *testing.T) {
	c := testConfig()
	c.ReplicationUpdateInterval = 864000
	_ = RenderEnvFile(c)
	c.ReplicationUpdateInterval = 300

	out := RenderEnvFile(c)
	if !strings.Contains(out, "NOMINATIM_REPLICATION_UPDATE_INTERVAL=300\n") {
		t.Fatalf("interval not replaced cleanly:\n%s", out)
	}
	if strings.Contains(out, "3000") {
		t.Fatalf("interval was spliced rather than replaced:\n%s", out)
	}
}
