package ctl

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Dataset struct {
	EnvVar string
	Remote string
	Local  string
	Label  string
}

var Datasets = []Dataset{
	{"IMPORT_WIKIPEDIA", "wikimedia-importance.csv.gz", "wikimedia-importance.csv.gz", "Wikipedia importance"},
	{"IMPORT_SECONDARY_WIKIPEDIA", "wikimedia-secondary-importance.sql.gz", "secondary_importance.sql.gz", "Wikipedia secondary importance"},
	{"IMPORT_GB_POSTCODES", "gb_postcodes.csv.gz", "gb_postcodes.csv.gz", "GB postcodes"},
	{"IMPORT_US_POSTCODES", "us_postcodes.csv.gz", "us_postcodes.csv.gz", "US postcodes"},
	{"IMPORT_TIGER_ADDRESSES", "tiger2024-nominatim-preprocessed.csv.tar.gz", "tiger-nominatim-preprocessed.csv.tar.gz", "Tiger addresses"},
}

type Config struct {
	ProjectDir string
	UserAgent  string
	Debug      bool

	PBFURL  string
	PBFPath string

	DatasetValues map[string]string

	MirrorBaseURL string

	PostgresHost      string
	PostgresPort      int
	PostgresDB        string
	PostgresSSLMode   string
	NominatimPassword string
	AdminPassword     string
	WebUser           string
	WebUserPassword   string

	ImportStyle         string
	ReverseOnly         bool
	Freeze              bool
	Threads             int
	AllowDropExistingDB bool

	RoleOptions         string
	ProvisionExtensions bool

	ReplicationURL             string
	ReplicationUpdateInterval  int // seconds
	ReplicationRecheckInterval int // seconds
	UpdateMode                 string

	GunicornBind            string
	GunicornWorkers         int
	GunicornTimeout         int // seconds
	GunicornGracefulTimeout int // seconds
	WarmupOnStartup         bool

	FlatnodeFile string
}

const adminUser = "postgres"

const (
	defaultMirror          = "https://nominatim.org/data"
	defaultUpdateInterval  = 86400
	defaultRecheckInterval = 900
)

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

	sums := []string{"PBF_SHA256"}
	for _, d := range Datasets {
		c.DatasetValues[d.EnvVar] = os.Getenv(d.EnvVar)
		sums = append(sums, d.EnvVar+"_SHA256")
	}
	for _, name := range sums {
		v := os.Getenv(name)
		if _, err := hex.DecodeString(v); v != "" && (len(v) != 64 || err != nil) {
			return nil, fmt.Errorf("%s must be the 64 hex digits of a SHA-256 digest (the first field of sha256sum output), got %q", name, v)
		}
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
		Logf("WARNING: NOMINATIM_WEBUSER_PASSWORD is not set; the read-only %s role shares NOMINATIM_PASSWORD", c.WebUser)
		c.WebUserPassword = c.NominatimPassword
	}

	cpus := runtime.GOMAXPROCS(0)
	if c.Threads, err = envInt("THREADS", cpus); err != nil {
		return nil, err
	}
	if c.GunicornWorkers, err = envInt("GUNICORN_WORKERS", cpus); err != nil {
		return nil, err
	}
	if c.GunicornTimeout, err = envInt("GUNICORN_TIMEOUT", 60); err != nil {
		return nil, err
	}
	if c.GunicornGracefulTimeout, err = envInt("GUNICORN_GRACEFUL_TIMEOUT", 30); err != nil {
		return nil, err
	}

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

func (c *Config) Validate() error {
	if !filepath.IsAbs(c.ProjectDir) {
		return fmt.Errorf("PROJECT_DIR must be an absolute path, got %q", c.ProjectDir)
	}
	if c.NominatimPassword == "" {
		return fmt.Errorf("NOMINATIM_PASSWORD must be set (no default is shipped; use NOMINATIM_PASSWORD_FILE for a secret file)")
	}
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
	if c.GunicornTimeout < 0 {
		return fmt.Errorf("GUNICORN_TIMEOUT must be >= 0, got %d", c.GunicornTimeout)
	}
	if c.GunicornGracefulTimeout < 0 {
		return fmt.Errorf("GUNICORN_GRACEFUL_TIMEOUT must be >= 0, got %d", c.GunicornGracefulTimeout)
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

func (c *Config) ShutdownGrace() time.Duration {
	return time.Duration(c.GunicornGracefulTimeout)*time.Second + 5*time.Second
}

func (c *Config) OSMFile() string {
	if c.PBFPath != "" {
		return c.PBFPath
	}
	return filepath.Join(c.ProjectDir, "data.osm.pbf")
}

func (c *Config) TigerEnabled() bool {
	v := c.DatasetValues["IMPORT_TIGER_ADDRESSES"]
	return v != "" && v != "false"
}

func (c *Config) EnvFilePath() string { return filepath.Join(c.ProjectDir, ".env") }

func (c *Config) DSN(user, password string) string {
	return fmt.Sprintf("pgsql:host=%s;port=%d;user=%s;password=%s;dbname=%s;sslmode=%s",
		c.PostgresHost, c.PostgresPort, user, password, c.PostgresDB, c.PostgresSSLMode)
}

func (c *Config) LibpqURL(user, password, database string) string {
	if database == "" {
		database = "postgres"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, saslprep(password)),
		Host:     net.JoinHostPort(c.PostgresHost, strconv.Itoa(c.PostgresPort)),
		Path:     "/" + database,
		RawQuery: "sslmode=" + c.PostgresSSLMode + "&application_name=nominatim-ctl",
	}
	return u.String()
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

func RenderEnvFile(c *Config) string {
	kv := map[string]string{
		"NOMINATIM_TOKENIZER":                    "icu",
		"NOMINATIM_REPLICATION_URL":              c.ReplicationURL,
		"NOMINATIM_REPLICATION_UPDATE_INTERVAL":  fmt.Sprint(c.ReplicationUpdateInterval),
		"NOMINATIM_REPLICATION_RECHECK_INTERVAL": fmt.Sprint(c.ReplicationRecheckInterval),
		"NOMINATIM_IMPORT_STYLE":                 c.ImportStyle,
		"NOMINATIM_FLATNODE_FILE":                c.FlatnodeFile,
		"NOMINATIM_DATABASE_DSN":                 c.DSN(c.WebUser, c.WebUserPassword),
		"NOMINATIM_DATABASE_WEBUSER":             c.WebUser,
	}
	if c.TigerEnabled() {
		kv["NOMINATIM_USE_US_TIGER_DATA"] = "yes"
	}

	var b strings.Builder
	b.WriteString("# Generated by nominatim-ctl on every container start. Edits are overwritten.\n")
	for _, k := range slices.Sorted(maps.Keys(kv)) {
		fmt.Fprintf(&b, "%s=%s\n", k, kv[k])
	}
	return b.String()
}

func WriteEnvFile(c *Config, uid, gid int) error {
	path := c.EnvFilePath()
	stale, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".env.tmp-*"))
	for _, p := range stale {
		if fi, err := os.Lstat(p); err == nil && time.Since(fi.ModTime()) > time.Minute {
			_ = os.Remove(p)
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".env.tmp-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(f.Name())
	if os.Geteuid() == 0 {
		if err := f.Chown(uid, gid); err != nil {
			f.Close()
			return fmt.Errorf("chown %s: %w", path, err)
		}
	}
	if _, err := f.WriteString(RenderEnvFile(c)); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	err = os.Rename(f.Name(), path)
	if errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("cannot replace %s: it is bind-mounted. It is generated from the environment on every start and must not be mounted; set environment variables instead", path)
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
