package ctl

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Dataset is one optional supplementary dataset Nominatim can consume during
// import. The env var is dual-typed for backwards compatibility with the shell
// implementation: "true" downloads the file, an existing path links it.
type Dataset struct {
	EnvVar string
	Remote string // file name on the mirror
	Local  string // file name Nominatim expects in the project directory
	Label  string
}

// Datasets is the full set, in the order the shell implementation fetched them.
var Datasets = []Dataset{
	{"IMPORT_WIKIPEDIA", "wikimedia-importance.csv.gz", "wikimedia-importance.csv.gz", "Wikipedia importance"},
	{"IMPORT_SECONDARY_WIKIPEDIA", "wikimedia-secondary-importance.sql.gz", "secondary_importance.sql.gz", "Wikipedia secondary importance"},
	{"IMPORT_GB_POSTCODES", "gb_postcodes.csv.gz", "gb_postcodes.csv.gz", "GB postcodes"},
	{"IMPORT_US_POSTCODES", "us_postcodes.csv.gz", "us_postcodes.csv.gz", "US postcodes"},
	{"IMPORT_TIGER_ADDRESSES", "tiger2024-nominatim-preprocessed.csv.tar.gz", "tiger-nominatim-preprocessed.csv.tar.gz", "Tiger addresses"},
}

// Config is the fully resolved runtime configuration. Every field is derived
// once, at startup, from the environment; nothing else reads os.Getenv.
type Config struct {
	ProjectDir string
	UserAgent  string
	Debug      bool

	// Data source. Exactly one of PBFURL / PBFPath must be set when an import
	// is required.
	PBFURL  string
	PBFPath string

	// Supplementary datasets, keyed by env var name. The value is the raw
	// operator-supplied string ("true", a path, or "").
	DatasetValues map[string]string

	MirrorBaseURL string

	// Database.
	PostgresHost      string
	PostgresPort      int
	PostgresDB        string
	PostgresSSLMode   string
	NominatimPassword string
	AdminPassword     string
	WebUser           string
	// WebUserPassword is separate so that leaking the API's DSN does not also
	// hand over the CREATEDB application role.
	WebUserPassword string

	// Import behaviour.
	ImportStyle         string
	ReverseOnly         bool
	Freeze              bool
	Threads             int
	AllowDropExistingDB bool

	// RoleOptions are the CREATE ROLE attributes for the application role.
	// CREATEDB is everything the import needs once PostGIS is pre-installed;
	// set NOMINATIM_ROLE_OPTIONS=SUPERUSER to restore the previous behaviour.
	RoleOptions string
	// ProvisionExtensions installs PostGIS and hstore into template1 so the
	// unprivileged application role inherits them in the database it creates.
	// Turn it off when a managed provider installs extensions for you.
	ProvisionExtensions bool

	// Replication.
	ReplicationURL             string
	ReplicationUpdateInterval  int
	ReplicationRecheckInterval int
	UpdateMode                 string

	// Serving.
	GunicornBind    string
	GunicornWorkers int
	WarmupOnStartup bool

	// FixVolumeOwnership recursively takes ownership of the project directory,
	// for migrating a volume written by the pre-refactor image.
	FixVolumeOwnership bool

	// Derived paths.
	FlatnodeFile string
}

// adminUser is the bootstrap superuser. Every PostgreSQL deployment this image
// targets calls it "postgres".
const adminUser = "postgres"

const (
	defaultMirror          = "https://nominatim.org/data"
	defaultUpdateInterval  = 86400
	defaultRecheckInterval = 900
)

// Load resolves the configuration from the process environment. It performs no
// I/O beyond reading *_FILE secret files and stat-ing the flatnode directory.
func Load() (*Config, error) {
	c := &Config{
		ProjectDir:                 envOr("PROJECT_DIR", "/nominatim"),
		UserAgent:                  envOr("USER_AGENT", "nominatim-docker"),
		Debug:                      envBool("DEBUG_MODE"),
		PBFURL:                     os.Getenv("PBF_URL"),
		PBFPath:                    os.Getenv("PBF_PATH"),
		DatasetValues:              map[string]string{},
		MirrorBaseURL:              strings.TrimRight(envOr("DATA_MIRROR_URL", defaultMirror), "/"),
		PostgresHost:               envOr("POSTGRES_HOST", "postgres"),
		PostgresDB:                 envOr("POSTGRES_DB", "nominatim"),
		PostgresSSLMode:            envOr("POSTGRES_SSLMODE", "prefer"),
		WebUser:                    envOr("NOMINATIM_WEBUSER", "www-data"),
		ImportStyle:                envOr("IMPORT_STYLE", "full"),
		RoleOptions:                envOr("NOMINATIM_ROLE_OPTIONS", "CREATEDB"),
		ProvisionExtensions:        envOr("PROVISION_EXTENSIONS", "true") == "true",
		ReverseOnly:                envBool("REVERSE_ONLY"),
		Freeze:                     envBool("FREEZE"),
		AllowDropExistingDB:        envBool("ALLOW_DROP_EXISTING_DB"),
		ReplicationURL:             os.Getenv("REPLICATION_URL"),
		ReplicationUpdateInterval:  defaultUpdateInterval,
		ReplicationRecheckInterval: defaultRecheckInterval,
		UpdateMode:                 os.Getenv("UPDATE_MODE"),
		GunicornBind:               envOr("GUNICORN_BIND", "0.0.0.0:8080"),
		WarmupOnStartup:            envBool("WARMUP_ON_STARTUP"),
		FixVolumeOwnership:         envBool("FIX_VOLUME_OWNERSHIP"),
	}

	for _, d := range Datasets {
		c.DatasetValues[d.EnvVar] = os.Getenv(d.EnvVar)
	}

	var err error
	if c.PostgresPort, err = envInt("POSTGRES_PORT", 5432); err != nil {
		return nil, err
	}
	if c.NominatimPassword, err = envSecret("NOMINATIM_PASSWORD"); err != nil {
		return nil, err
	}
	if c.AdminPassword, err = envSecret("POSTGRES_ADMIN_PASSWORD"); err != nil {
		return nil, err
	}
	if c.WebUserPassword, err = envSecret("NOMINATIM_WEBUSER_PASSWORD"); err != nil {
		return nil, err
	}
	if c.WebUserPassword == "" {
		// Backwards compatible: one password still works, but the roles are
		// only really separated when the web role has its own.
		c.WebUserPassword = c.NominatimPassword
	}

	// Threads and worker count default to the CPU allowance actually granted to
	// this container, not to the host core count.
	cpus := availableCPUs()
	if c.Threads, err = envInt("THREADS", cpus); err != nil {
		return nil, err
	}
	if c.GunicornWorkers, err = envInt("GUNICORN_WORKERS", cpus); err != nil {
		return nil, err
	}

	// The replication intervals are only meaningful alongside a replication URL.
	// Preserve the shell implementation's hard failure on that combination.
	if raw := os.Getenv("REPLICATION_UPDATE_INTERVAL"); raw != "" {
		if c.ReplicationUpdateInterval, err = parseInterval("REPLICATION_UPDATE_INTERVAL", raw, c.ReplicationURL); err != nil {
			return nil, err
		}
	}
	if raw := os.Getenv("REPLICATION_RECHECK_INTERVAL"); raw != "" {
		if c.ReplicationRecheckInterval, err = parseInterval("REPLICATION_RECHECK_INTERVAL", raw, c.ReplicationURL); err != nil {
			return nil, err
		}
	}

	// A flatnode directory mounted into the project dir opts the import into
	// flatnode storage, matching the previous behaviour.
	flatnodeDir := filepath.Join(c.ProjectDir, "flatnode")
	if fi, statErr := os.Stat(flatnodeDir); statErr == nil && fi.IsDir() {
		c.FlatnodeFile = filepath.Join(flatnodeDir, "flatnode.file")
	}

	return c, c.Validate()
}

func parseInterval(name, raw, replicationURL string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, raw)
	}
	if replicationURL == "" {
		return 0, fmt.Errorf("%s requires REPLICATION_URL to be set", name)
	}
	return n, nil
}

// Validate reports configuration that cannot produce a working container.
// Checks that only matter when an import will actually run live in
// ValidateForImport instead, so that a restart of an already-imported database
// does not require the original PBF settings to still be present.
func (c *Config) Validate() error {
	if c.ProjectDir == "" {
		return fmt.Errorf("PROJECT_DIR must not be empty")
	}
	if !filepath.IsAbs(c.ProjectDir) {
		return fmt.Errorf("PROJECT_DIR must be an absolute path, got %q", c.ProjectDir)
	}
	if c.NominatimPassword == "" {
		return fmt.Errorf("NOMINATIM_PASSWORD must be set (no default is shipped; use NOMINATIM_PASSWORD_FILE for a secret file)")
	}
	// Nominatim parses its own pgsql: DSN by splitting fields on ';' and each
	// field on '=', so either character silently corrupts the connection string.
	for name, pw := range map[string]string{"NOMINATIM_PASSWORD": c.NominatimPassword, "NOMINATIM_WEBUSER_PASSWORD": c.WebUserPassword} {
		if strings.ContainsAny(pw, ";=") {
			return fmt.Errorf("%s must not contain ';' or '=' (both are field separators in the Nominatim DSN)", name)
		}
	}
	if c.PostgresPort < 1 || c.PostgresPort > 65535 {
		return fmt.Errorf("POSTGRES_PORT must be 1-65535, got %d", c.PostgresPort)
	}
	if c.PostgresDB == "" {
		return fmt.Errorf("POSTGRES_DB must not be empty")
	}
	if c.Threads < 1 {
		return fmt.Errorf("THREADS must be >= 1, got %d", c.Threads)
	}
	if c.GunicornWorkers < 1 {
		return fmt.Errorf("GUNICORN_WORKERS must be >= 1, got %d", c.GunicornWorkers)
	}
	switch c.UpdateMode {
	case "", "continuous", "once", "catch-up":
	default:
		return fmt.Errorf("UPDATE_MODE must be one of continuous, once, catch-up (got %q)", c.UpdateMode)
	}
	switch c.PostgresSSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return fmt.Errorf("POSTGRES_SSLMODE %q is not a libpq sslmode", c.PostgresSSLMode)
	}
	for _, d := range Datasets {
		v := c.DatasetValues[d.EnvVar]
		if v == "" || v == "true" || v == "false" {
			continue
		}
		if !filepath.IsAbs(v) {
			return fmt.Errorf("%s must be \"true\", \"false\", or an absolute path (got %q)", d.EnvVar, v)
		}
		if _, err := os.Stat(v); err != nil {
			return fmt.Errorf("%s points at %q which cannot be read: %w", d.EnvVar, v, err)
		}
	}
	return nil
}

// ValidateForImport adds the checks that only apply when a fresh import is
// about to run.
func (c *Config) ValidateForImport() error {
	switch {
	case c.PBFURL == "" && c.PBFPath == "":
		return fmt.Errorf("an import is required but neither PBF_URL nor PBF_PATH is set")
	case c.PBFURL != "" && c.PBFPath != "":
		return fmt.Errorf("PBF_URL and PBF_PATH are mutually exclusive; set exactly one")
	}
	if c.AdminPassword == "" {
		return fmt.Errorf("POSTGRES_ADMIN_PASSWORD must be set to provision the database (it is never derived from NOMINATIM_PASSWORD)")
	}
	if c.PBFPath != "" {
		if _, err := os.Stat(c.PBFPath); err != nil {
			return fmt.Errorf("PBF_PATH %q cannot be read: %w", c.PBFPath, err)
		}
	}
	return nil
}

// OSMFile is the path the import reads the extract from.
func (c *Config) OSMFile() string {
	if c.PBFPath != "" {
		return c.PBFPath
	}
	return filepath.Join(c.ProjectDir, "data.osm.pbf")
}

// TigerEnabled reports whether Tiger address data should be wired up.
func (c *Config) TigerEnabled() bool {
	v := c.DatasetValues["IMPORT_TIGER_ADDRESSES"]
	return v != "" && v != "false"
}

// EnvFilePath is the Nominatim project configuration file.
func (c *Config) EnvFilePath() string { return filepath.Join(c.ProjectDir, ".env") }

// DSN builds a Nominatim-style connection string for the given role.
func (c *Config) DSN(user, password string) string {
	return fmt.Sprintf("pgsql:host=%s;port=%d;user=%s;password=%s;dbname=%s;sslmode=%s",
		c.PostgresHost, c.PostgresPort, user, password, c.PostgresDB, c.PostgresSSLMode)
}

// LibpqURL builds a URL the pgx driver understands. database may be empty to
// connect to the maintenance database.
//
// The password is SASLprep'd here because three implementations disagree on how
// to normalise it: PostgreSQL uses RFC 4013 SASLprep (NFKC, ignorables removed),
// pgx uses precis.OpaqueString (NFC, ignorables rejected), and the verifier this
// package stores uses RFC 4013. Handing pgx an already-prepared password makes
// its own pass a no-op, so all three agree. Nominatim keeps the raw password in
// its DSN — libpq applies RFC 4013 itself.
func (c *Config) LibpqURL(user, password, database string) string {
	if database == "" {
		database = "postgres"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s&application_name=nominatim-ctl",
		urlEscape(user), urlEscape(saslprep(password)), c.PostgresHost, c.PostgresPort,
		urlEscape(database), c.PostgresSSLMode)
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envBool(name string) bool { return os.Getenv(name) == "true" }

func envInt(name string, def int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, raw)
	}
	return n, nil
}

// envSecret reads NAME, or the contents of the file named by NAME_FILE. The
// file form keeps secrets out of `docker inspect` and the process environment
// of every child.
func envSecret(name string) (string, error) {
	if p := os.Getenv(name + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("reading %s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv(name), nil
}

// availableCPUs returns the number of CPUs this container may actually use.
//
// runtime.NumCPU (like nproc) honours the CPU affinity mask but is blind to the
// CFS quota, so `--cpus=2` on a 64-core host reports 64. Sizing osm2pgsql
// threads and Gunicorn workers off that number oversubscribes the container and
// exhausts the database connection limit.
//
// cgroup v2 only: every kernel that can run this image exposes cpu.max.
func availableCPUs() int {
	n := runtime.NumCPU()
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		// cgroup v1 host: the quota lives elsewhere and is not read. Say so,
		// because a --cpus limit will otherwise be silently oversubscribed.
		Logf("note: no cgroup v2 cpu.max; sizing from %d host CPUs. "+
			"Set THREADS and GUNICORN_WORKERS explicitly if this container is CPU-limited.", n)
		return n
	}
	if q := parseCPUMax(string(b)); q > 0 && q < n {
		n = q
	}
	if n < 1 {
		n = 1
	}
	return n
}

// parseCPUMax reads "<quota> <period>", or "max <period>" when unlimited, and
// returns the quota rounded up to whole CPUs.
func parseCPUMax(s string) int {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.Atoi(f[0])
	period, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return int(math.Ceil(float64(quota) / float64(period)))
}

// RenderEnvFile builds the contents of the Nominatim project .env.
//
// The file is regenerated in full on every start rather than patched in place.
// The previous implementation substituted __PLACEHOLDER__ tokens with sed, which
// consumed the placeholders on the first run: on a persisted volume every later
// start silently ignored POSTGRES_HOST, NOMINATIM_PASSWORD, IMPORT_STYLE and the
// replication intervals. Regenerating is idempotent by construction, so the
// container's configuration always matches its environment.
func RenderEnvFile(c *Config) string {
	kv := map[string]string{
		"NOMINATIM_TOKENIZER":                    "icu",
		"NOMINATIM_REPLICATION_URL":              c.ReplicationURL,
		"NOMINATIM_REPLICATION_UPDATE_INTERVAL":  fmt.Sprint(c.ReplicationUpdateInterval),
		"NOMINATIM_REPLICATION_RECHECK_INTERVAL": fmt.Sprint(c.ReplicationRecheckInterval),
		"NOMINATIM_IMPORT_STYLE":                 c.ImportStyle,
		"NOMINATIM_FLATNODE_FILE":                c.FlatnodeFile,
		"NOMINATIM_DATABASE_DSN":                 c.DSN("nominatim", c.NominatimPassword),
		"NOMINATIM_DATABASE_WEBUSER":             c.WebUser,
	}
	if c.TigerEnabled() {
		kv["NOMINATIM_USE_US_TIGER_DATA"] = "yes"
	}

	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# Generated by nominatim-ctl on every container start. Edits are overwritten.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, kv[k])
	}
	return b.String()
}

// WriteEnvFile renders and writes the project .env with owner-only permissions.
// The file holds a database password in cleartext, so 0600 matters: with the
// old 0644 any process in the container — including a compromised Gunicorn
// worker — could read it, and on a bind mount so could every host user.
func WriteEnvFile(c *Config, uid, gid int) error {
	path := c.EnvFilePath()
	if err := os.WriteFile(path, []byte(RenderEnvFile(c)), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// WriteFile applies its mode only when creating, so a volume carrying a
	// 0644 .env from an older image would keep it.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", path, err)
		}
	}
	return nil
}
