package ctl

import (
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func baseEnv() map[string]string {
	return map[string]string{
		"NOMINATIM_PASSWORD":      "s3cret",
		"POSTGRES_ADMIN_PASSWORD": "admin-pw",
		"POSTGRES_HOST":           "db",
		"PBF_URL":                 "https://example.invalid/monaco.osm.pbf",
		"PROJECT_DIR":             "/nominatim",
		"THREADS":                 "4",
		"GUNICORN_WORKERS":        "4",
	}
}

func TestLoadDefaults(t *testing.T) {
	withEnv(t, baseEnv())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PostgresPort != 5432 || c.PostgresDB != "nominatim" || c.WebUser != "www-data" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.ReplicationUpdateInterval != 86400 || c.ReplicationRecheckInterval != 900 {
		t.Fatalf("unexpected replication defaults: %+v", c)
	}
	if c.ImportStyle != "full" {
		t.Fatalf("IMPORT_STYLE default = %q, want full", c.ImportStyle)
	}
}

// A password with no default is the whole point: the previous image shipped one
// baked into the Dockerfile and used it as a PostgreSQL superuser password.
func TestPasswordIsRequired(t *testing.T) {
	env := baseEnv()
	delete(env, "NOMINATIM_PASSWORD")
	withEnv(t, env)
	t.Setenv("NOMINATIM_PASSWORD", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected Load to fail without NOMINATIM_PASSWORD")
	}
}

func TestPasswordFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pw")
	if err := os.WriteFile(p, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withEnv(t, baseEnv())
	t.Setenv("NOMINATIM_PASSWORD", "")
	t.Setenv("NOMINATIM_PASSWORD_FILE", p)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.NominatimPassword != "from-file" {
		t.Fatalf("password = %q, want from-file", c.NominatimPassword)
	}
}

// Nominatim splits its own DSN on ';', so such a password would silently
// truncate the connection string rather than fail.
func TestPasswordRejectsDSNSeparator(t *testing.T) {
	withEnv(t, baseEnv())
	t.Setenv("NOMINATIM_PASSWORD", "pass;word")
	if _, err := Load(); err == nil {
		t.Fatal("expected rejection of ';' in NOMINATIM_PASSWORD")
	}
}

func TestPBFMutualExclusion(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"both":    {"PBF_URL": "https://x.invalid/a.pbf", "PBF_PATH": "/tmp/a.pbf"},
		"neither": {"PBF_URL": "", "PBF_PATH": ""},
	} {
		t.Run(name, func(t *testing.T) {
			withEnv(t, baseEnv())
			for k, v := range env {
				t.Setenv(k, v)
			}
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			// Load itself succeeds: a restart of an already-imported database
			// must not require the original PBF settings.
			if err := c.ValidateForImport(); err == nil {
				t.Fatal("expected ValidateForImport to reject the combination")
			}
		})
	}
}

// The admin password must never be inferred from the application password.
func TestAdminPasswordNotDerived(t *testing.T) {
	env := baseEnv()
	delete(env, "POSTGRES_ADMIN_PASSWORD")
	withEnv(t, env)
	t.Setenv("POSTGRES_ADMIN_PASSWORD", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AdminPassword == c.NominatimPassword {
		t.Fatal("admin password must not default to the application password")
	}
	if err := c.ValidateForImport(); err == nil {
		t.Fatal("expected ValidateForImport to require POSTGRES_ADMIN_PASSWORD")
	}
}

func TestReplicationIntervalRequiresURL(t *testing.T) {
	withEnv(t, baseEnv())
	t.Setenv("REPLICATION_UPDATE_INTERVAL", "300")
	if _, err := Load(); err == nil {
		t.Fatal("expected failure when interval is set without REPLICATION_URL")
	}

	t.Setenv("REPLICATION_URL", "https://example.invalid/replication")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ReplicationUpdateInterval != 300 {
		t.Fatalf("interval = %d, want 300", c.ReplicationUpdateInterval)
	}
}

func TestInvalidUpdateModeRejected(t *testing.T) {
	withEnv(t, baseEnv())
	t.Setenv("UPDATE_MODE", "contineous") // realistic typo
	if _, err := Load(); err == nil {
		t.Fatal("expected an unknown UPDATE_MODE to be rejected")
	}
}

// The shell version silently skipped a relative or misspelled dataset value,
// producing an import quietly missing its importance data.
func TestDatasetPathMustBeAbsoluteAndExist(t *testing.T) {
	withEnv(t, baseEnv())
	t.Setenv("IMPORT_WIKIPEDIA", "data/wiki.csv.gz")
	if _, err := Load(); err == nil {
		t.Fatal("expected a relative dataset path to be rejected")
	}

	t.Setenv("IMPORT_WIKIPEDIA", "/nonexistent/wiki.csv.gz")
	if _, err := Load(); err == nil {
		t.Fatal("expected a missing dataset file to be rejected")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "wiki.csv.gz")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IMPORT_WIKIPEDIA", p)
	if _, err := Load(); err != nil {
		t.Fatalf("absolute existing path rejected: %v", err)
	}
}

func TestTigerEnabled(t *testing.T) {
	withEnv(t, baseEnv())
	c, _ := Load()
	if c.TigerEnabled() {
		t.Fatal("Tiger should be off by default")
	}
	t.Setenv("IMPORT_TIGER_ADDRESSES", "true")
	c, _ = Load()
	if !c.TigerEnabled() {
		t.Fatal("Tiger should be enabled")
	}
	t.Setenv("IMPORT_TIGER_ADDRESSES", "false")
	c, _ = Load()
	if c.TigerEnabled() {
		t.Fatal(`"false" must not enable Tiger`)
	}
}

func TestDSNIncludesSSLMode(t *testing.T) {
	withEnv(t, baseEnv())
	t.Setenv("POSTGRES_SSLMODE", "require")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dsn := c.DSN("nominatim", "pw")
	if !strings.Contains(dsn, "sslmode=require") {
		t.Fatalf("DSN missing sslmode: %s", dsn)
	}
}

func TestInvalidSSLModeRejected(t *testing.T) {
	withEnv(t, baseEnv())
	t.Setenv("POSTGRES_SSLMODE", "yes-please")
	if _, err := Load(); err == nil {
		t.Fatal("expected an invalid sslmode to be rejected")
	}
}

// url.QueryEscape encodes a space as "+", and the userinfo decoder in net/url
// does not map it back. The role password would be set correctly and then fail
// every subsequent login.
func TestLibpqURLSurvivesSpecialCharactersInPassword(t *testing.T) {
	withEnv(t, baseEnv())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, pw := range []string{"my pass", "p@ss/word", "a+b", "%41", "pä ss"} {
		u := c.LibpqURL("nominatim", pw, "nominatim")
		parsed, err := neturl.Parse(u)
		if err != nil {
			t.Fatalf("LibpqURL(%q) is not parseable: %v", pw, err)
		}
		got, _ := parsed.User.Password()
		if got != pw {
			t.Errorf("password round-trip failed: put %q, got %q (url %s)", pw, got, u)
		}
	}
}

// Both characters are field separators in Nominatim's own pgsql: DSN parser.
func TestPasswordRejectsDSNMetacharacters(t *testing.T) {
	for _, pw := range []string{"pass;word", "pass=word"} {
		withEnv(t, baseEnv())
		t.Setenv("NOMINATIM_PASSWORD", pw)
		if _, err := Load(); err == nil {
			t.Errorf("expected %q to be rejected", pw)
		}
	}
}

// A separate web-role password is what actually separates the privileges.
func TestWebUserPasswordDefaultsButCanDiffer(t *testing.T) {
	withEnv(t, baseEnv())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.WebUserPassword != c.NominatimPassword {
		t.Fatal("web password should fall back to NOMINATIM_PASSWORD")
	}

	t.Setenv("NOMINATIM_WEBUSER_PASSWORD", "distinct-web-pw")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.WebUserPassword != "distinct-web-pw" || c.WebUserPassword == c.NominatimPassword {
		t.Fatalf("web password = %q", c.WebUserPassword)
	}
}
