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

// Dataset is one optional supplementary dataset. The env var is dual-typed:
// "true" downloads the file from the mirror, an absolute path links it.
type Dataset struct {
	EnvVar string
	Remote string // file name on the mirror
	Local  string // file name Nominatim expects in the project directory
	Label  string
}

// Datasets is the full set of supplementary datasets.
var Datasets = []Dataset{
	{"IMPORT_WIKIPEDIA", "wikimedia-importance.csv.gz", "wikimedia-importance.csv.gz", "Wikipedia importance"},
	{"IMPORT_SECONDARY_WIKIPEDIA", "wikimedia-secondary-importance.sql.gz", "secondary_importance.sql.gz", "Wikipedia secondary importance"},
	{"IMPORT_GB_POSTCODES", "gb_postcodes.csv.gz", "gb_postcodes.csv.gz", "GB postcodes"},
	{"IMPORT_US_POSTCODES", "us_postcodes.csv.gz", "us_postcodes.csv.gz", "US postcodes"},
	{"IMPORT_TIGER_ADDRESSES", "tiger2024-nominatim-preprocessed.csv.tar.gz", "tiger-nominatim-preprocessed.csv.tar.gz", "Tiger addresses"},
}

// Config is the fully resolved runtime configuration, derived once at startup
// from the environment; nothing else reads os.Getenv.
type Config struct {
	ProjectDir string
	UserAgent  string
	Debug      bool

	// Data source: exactly one of PBFURL / PBFPath when an import runs.
	PBFURL  string
	PBFPath string

	// Raw operator values ("true", a path, or ""), keyed by env var name.
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
	// Separate so that leaking the API's DSN does not also hand over the
	// CREATEDB application role.
	WebUserPassword string

	// Import behaviour.
	ImportStyle         string
	ReverseOnly         bool
	Freeze              bool
	Threads             int
	AllowDropExistingDB bool

	// RoleOptions are the CREATE ROLE attributes for the application role;
	// CREATEDB suffices once PostGIS is pre-installed.
	RoleOptions string
	// ProvisionExtensions installs PostGIS/hstore into template1 so the
	// unprivileged role inherits them. Off when a managed provider does it.
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

	// Derived paths.
	FlatnodeFile string
}

// adminUser is the bootstrap superuser.
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
		// One password still works; the roles are only really separated when
		// the web role has its own.
		c.WebUserPassword = c.NominatimPassword
	}

	// Sized from the CPU allowance actually granted to this container.
	cpus := availableCPUs()
	if c.Threads, err = envInt("THREADS", cpus); err != nil {
		return nil, err
	}
	if c.GunicornWorkers, err = envInt("GUNICORN_WORKERS", cpus); err != nil {
		return nil, err
	}

	// Intervals are only meaningful alongside a replication URL.
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

	// A mounted flatnode directory opts the import into flatnode storage.
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
// Import-only checks live in ValidateForImport, so a restart of an
// already-imported database does not require the original PBF settings.
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
	// Nominatim splits its DSN on ';' and each field on '=', so either
	// character silently corrupts the connection string.
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

// ValidateForImport adds the checks that only apply when a fresh import runs.
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

// LibpqURL builds a URL for the pgx driver; database may be empty for the
// maintenance database.
//
// The password is SASLprep'd here so all three normalisers agree: PostgreSQL
// and the stored verifier use RFC 4013, while pgx uses precis.OpaqueString —
// handing pgx an already-prepared password makes its own pass a no-op.
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

// envSecret reads NAME, or the contents of the file named by NAME_FILE, which
// keeps secrets out of `docker inspect` and child process environments.
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

// availableCPUs returns the CPUs this container may actually use.
// runtime.NumCPU honours the affinity mask but not the CFS quota, so
// `--cpus=2` on a 64-core host would otherwise oversubscribe the container
// and exhaust the database connection limit. cgroup v2 only.
func availableCPUs() int {
	n := runtime.NumCPU()
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
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

// parseCPUMax reads "<quota> <period>" ("max <period>" when unlimited) and
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

// RenderEnvFile builds the Nominatim project .env, regenerated in full on
// every start so the configuration always matches the environment.
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

// WriteEnvFile renders and writes the project .env with owner-only
// permissions: it holds a cleartext database password.
func WriteEnvFile(c *Config, uid, gid int) error {
	path := c.EnvFilePath()
	if err := os.WriteFile(path, []byte(RenderEnvFile(c)), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// WriteFile applies its mode only on create; correct a pre-existing file.
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
