package ctl

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
	withEnv(t, baseEnv())
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
	withEnv(t, baseEnv())
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

// Parsed by pgx itself, not net/url: pgx follows libpq, which ends the
// userinfo at the first '@' and keeps '+' literal. A mis-escaped password is
// set correctly on the role and then fails every subsequent login.
func TestLibpqURLSurvivesSpecialCharactersInPassword(t *testing.T) {
	withEnv(t, baseEnv())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct{ pw, db string }{
		{"my pass", "nominatim"}, {"p@ss/word", "nominatim"}, {"a+b", "nominatim"},
		{"%41", "nominatim"}, {"pä ss", "nominatim"}, {"a:b@c#d?e", "nominatim"},
		{"@:/%+ #?", "my db%@#?"},
	} {
		u := c.LibpqURL("nominatim", tc.pw, tc.db)
		cfg, err := pgconn.ParseConfig(u)
		if err != nil {
			t.Fatalf("LibpqURL(%q, %q) is not parseable by pgx: %v", tc.pw, tc.db, err)
		}
		if cfg.User != "nominatim" || cfg.Password != tc.pw || cfg.Database != tc.db || cfg.Host != "db" || cfg.Port != 5432 {
			t.Errorf("round-trip failed for password %q, database %q: got user %q password %q database %q host %q port %d (url %s)",
				tc.pw, tc.db, cfg.User, cfg.Password, cfg.Database, cfg.Host, cfg.Port, u)
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

func TestThreadsDefaultToGOMAXPROCS(t *testing.T) {
	env := baseEnv()
	delete(env, "THREADS")
	delete(env, "GUNICORN_WORKERS")
	withEnv(t, env)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := runtime.GOMAXPROCS(0); c.Threads != want || c.GunicornWorkers != want {
		t.Fatalf("THREADS=%d GUNICORN_WORKERS=%d, want %d", c.Threads, c.GunicornWorkers, want)
	}
}

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
	// The full image: its .env carries the application role that rotates here.
	defer func(f func() bool) { importToolsPresent = f }(importToolsPresent)
	importToolsPresent = func() bool { return true }
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

// .env sits in a directory the workload user can write to, and is rewritten
// as root on every start: a symlink there must be replaced, never followed.
func TestWriteEnvFileReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Config{ProjectDir: dir}
	if err := os.Symlink(target, c.EnvFilePath()); err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(c, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "untouched" {
		t.Fatalf("symlink target overwritten: %q", got)
	}
	fi, err := os.Lstat(c.EnvFilePath())
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".env = %v (%v), want a regular 0600 file", fi.Mode(), err)
	}
}

// An IPv6 literal must be bracketed, or the URL does not parse at all.
func TestLibpqURLBracketsIPv6(t *testing.T) {
	c := &Config{PostgresHost: "fd00::5", PostgresPort: 5432, PostgresSSLMode: "prefer"}
	u, err := url.Parse(c.LibpqURL("nominatim", "pw", "nominatim"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Hostname() != "fd00::5" || u.Port() != "5432" {
		t.Fatalf("host %q port %q", u.Hostname(), u.Port())
	}
}

// The serve-only image's .env is readable by the API's uid, so it must not
// hold the owning role's password.
func TestRenderEnvFileServeImageUsesWebRole(t *testing.T) {
	withEnv(t, baseEnv())
	t.Setenv("NOMINATIM_WEBUSER_PASSWORD", "web-password-1")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer func(f func() bool) { importToolsPresent = f }(importToolsPresent)

	importToolsPresent = func() bool { return false }
	if env := RenderEnvFile(c); !strings.Contains(env, "user=www-data;password=web-password-1;") || strings.Contains(env, "user=nominatim;") {
		t.Fatalf("serve image .env must carry only the web role:\n%s", env)
	}
	importToolsPresent = func() bool { return true }
	if env := RenderEnvFile(c); !strings.Contains(env, "user=nominatim;") {
		t.Fatalf("full image .env must carry the application role:\n%s", env)
	}
}

// GUNICORN_GRACEFUL_TIMEOUT drives the SIGKILL deadline, so it must be a number.
func TestGracefulTimeout(t *testing.T) {
	withEnv(t, baseEnv())
	c, err := Load()
	if err != nil || c.GunicornGracefulTimeout != 30 {
		t.Fatalf("default = %v, %v; want 30", c, err)
	}
	for _, bad := range []string{"abc", "-1"} {
		t.Setenv("GUNICORN_GRACEFUL_TIMEOUT", bad)
		if _, err := Load(); err == nil {
			t.Errorf("GUNICORN_GRACEFUL_TIMEOUT=%q accepted", bad)
		}
	}
}
