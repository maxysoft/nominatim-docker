package ctl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// nominatimHome matches the account created in the Dockerfile.
const nominatimHome = "/var/lib/nominatim"

// BaseEnv is the environment handed to every child process: a fixed base plus
// every NOMINATIM_* and PG* variable from the container environment, so
// operators can tune Nominatim settings and supply libpq TLS material.
func BaseEnv(c *Config) []string {
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"HOME=" + nominatimHome,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PYTHONUNBUFFERED=1",
	}
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		// The DSN is owned by this process.
		if k == "NOMINATIM_DATABASE_DSN" {
			continue
		}
		if strings.HasPrefix(k, "NOMINATIM_") || strings.HasPrefix(k, "PG") {
			env = append(env, kv)
		}
	}
	// Appended last so it wins over anything inherited above.
	return append(env, "NOMINATIM_DATABASE_DSN="+c.DSN("nominatim", c.NominatimPassword))
}

// PrepareProjectDir creates the project directory, renders the configuration
// into it, and takes ownership of NOMINATIM_HOME — under a read-only root
// filesystem $HOME is a tmpfs mounted fresh, and root-owned, on every boot.
func PrepareProjectDir(c *Config, uid, gid int) error {
	if err := os.MkdirAll(c.ProjectDir, 0o755); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(nominatimHome, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", nominatimHome, err)
		}
	}
	return WriteEnvFile(c, uid, gid)
}

// Serve runs the full container lifecycle: configure, import if needed, then
// supervise the API server until it exits or the context is cancelled.
func Serve(ctx context.Context, c *Config) error {
	uid, gid, err := LookupNominatimUser()
	if err != nil {
		return err
	}
	r := &Runner{UID: uid, GID: gid, Dir: c.ProjectDir, Env: BaseEnv(c)}

	if err := PrepareProjectDir(c, uid, gid); err != nil {
		return err
	}
	if c.Debug {
		// The DSN line is withheld rather than relying on redaction:
		// RegisterSecret ignores values shorter than four characters.
		for _, line := range strings.Split(RenderEnvFile(c), "\n") {
			if line != "" && !strings.HasPrefix(line, "NOMINATIM_DATABASE_DSN=") {
				Logf("config: %s", line)
			}
		}
	}

	if err := ensureImported(ctx, c, r); err != nil {
		return err
	}

	appURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	if err := waitForDatabase(ctx, appURL, 150, 2*time.Second); err != nil {
		return err
	}

	if err := r.Run(ctx, "nominatim", "refresh", "--functions", "--project-dir", c.ProjectDir); err != nil {
		return err
	}

	replication, err := startReplication(ctx, c, r)
	if err != nil {
		return err
	}

	if c.WarmupOnStartup {
		if c.ReverseOnly {
			Logf("warming database caches for reverse queries")
		} else {
			Logf("warming database caches for search and reverse queries")
		}
		if err := warmCaches(ctx, c, r); err != nil {
			return err
		}
		Logf("Warming finished")
	} else {
		Logf("Skipping cache warmup")
	}

	return runGunicorn(ctx, c, r, replication)
}

// ensureImported decides whether an import is required, and runs one if so.
// The decision is made only after the server is known reachable — a booting
// database must not be mistaken for an empty one on a routine restart.
func ensureImported(ctx context.Context, c *Config, r *Runner) error {
	haveAdmin := c.AdminPassword != ""

	probeURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	if haveAdmin {
		probeURL = c.LibpqURL(adminUser, c.AdminPassword, "postgres")
	}
	Logf("waiting for PostgreSQL at %s:%d", c.PostgresHost, c.PostgresPort)
	if err := waitForDatabase(ctx, probeURL, 150, 2*time.Second); err != nil {
		if haveAdmin {
			return err
		}
		return fmt.Errorf("%w\n(no POSTGRES_ADMIN_PASSWORD is set, so the database cannot be provisioned either)", err)
	}

	targetURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	if haveAdmin {
		targetURL = c.LibpqURL(adminUser, c.AdminPassword, c.PostgresDB)
	}

	// A database we cannot open holds no import (the server itself is
	// reachable), but the reason is logged because the fallback is the import.
	var complete, hasData bool
	if conn, err := pgx.Connect(ctx, targetURL); err == nil {
		complete = importComplete(ctx, conn, c.PostgresDB)
		hasData = hasNominatimData(ctx, conn)
		conn.Close(ctx)
	} else {
		Logf("cannot inspect database %q (%s); assuming it holds no import", c.PostgresDB, Redact(err.Error()))
	}

	if complete {
		Logf("existing Nominatim import found in %q — skipping import", c.PostgresDB)
		return chownProjectFiles(c, r.UID, r.GID)
	}

	if hasData && !c.AllowDropExistingDB {
		// Tables without a completion marker: an import that died part-way, or
		// a database imported elsewhere. Nominatim's validator decides which.
		Logf("database %q holds Nominatim tables but no completion marker; validating", c.PostgresDB)
		if err := r.Run(ctx, "nominatim", "admin", "--check-database", "--project-dir", c.ProjectDir); err != nil {
			return fmt.Errorf("database %q contains an incomplete or invalid Nominatim schema: %w\n"+
				"Set ALLOW_DROP_EXISTING_DB=true to discard it and import again", c.PostgresDB, err)
		}
		Logf("validation passed; adopting the existing import")
		// The adopted database was not created here, so COMMENT ON DATABASE
		// may need the admin connection.
		if err := recordImport(ctx, targetURL, c.PostgresDB); err != nil {
			return err
		}
		return chownProjectFiles(c, r.UID, r.GID)
	}

	Logf("no completed Nominatim import in %q — running import", c.PostgresDB)
	if err := RunImport(ctx, c, r); err != nil {
		return err
	}
	// Recorded only after full success, so an interrupted run is retried.
	return recordImport(ctx, c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB), c.PostgresDB)
}

// recordImport stamps the completion marker; COMMENT ON DATABASE needs
// ownership, so the caller chooses which role connects.
func recordImport(ctx context.Context, url, dbname string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	return markImportComplete(ctx, conn, dbname)
}

// startReplication launches the background diff process, if configured.
func startReplication(ctx context.Context, c *Config, r *Runner) (*exec.Cmd, error) {
	if c.ReplicationURL == "" || c.Freeze {
		Logf("skipping replication")
		return nil, nil
	}
	// nominatim replication shells out to osm2pgsql for every diff, which the
	// serve-only image does not ship. An explicit UPDATE_MODE is a promise
	// this image cannot keep, so it fails rather than serving stale data.
	if !HaveImportTools() {
		if c.UpdateMode != "" {
			return nil, fmt.Errorf("UPDATE_MODE=%q needs osm2pgsql, which the serve-only image does not ship; run replication from the full image", c.UpdateMode)
		}
		Logf("serve-only image: skipping replication (no osm2pgsql)")
		return nil, nil
	}
	dl := NewDownloader(c.UserAgent)
	if !dl.Reachable(ctx, c.ReplicationURL, 3, 2*time.Second) {
		Logf("WARNING: REPLICATION_URL unreachable; skipping replication")
		return nil, nil
	}
	// Re-init on every start in case the replication settings changed; this
	// also keeps the state usable for a manual `nominatim replication --once`.
	if err := r.Run(ctx, "nominatim", "replication", "--init", "--project-dir", c.ProjectDir); err != nil {
		return nil, err
	}
	if c.UpdateMode == "" {
		Logf("no UPDATE_MODE set; not starting a background replication process")
		return nil, nil
	}

	args := []string{"replication", "--project-dir", c.ProjectDir}
	switch c.UpdateMode {
	case "once":
		args = append(args, "--once")
	case "catch-up":
		args = append(args, "--catch-up")
	}
	Logf("starting replication (%s)", c.UpdateMode)
	cmd := r.Command(ctx, "nominatim", args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting replication: %w", err)
	}
	// Reaped here: with UPDATE_MODE=once nothing else would ever wait on it.
	go func() { _ = cmd.Wait() }()
	return cmd, nil
}

// runGunicorn starts the API in the foreground and supervises it, so a crash
// exits non-zero while a signalled stop exits clean.
func runGunicorn(ctx context.Context, c *Config, r *Runner, replication *exec.Cmd) error {
	args := []string{
		"--bind", c.GunicornBind,
		"--workers", fmt.Sprint(c.GunicornWorkers),
		"--worker-class", "asgi",
		"--worker-tmp-dir", "/dev/shm",
		"--access-logfile", "-",
		"--error-logfile", "-",
		"--enable-stdio-inheritance",
		// Bounded request handling: a slow or oversized client must not hold a
		// worker forever, and workers recycle periodically.
		"--timeout", envOr("GUNICORN_TIMEOUT", "60"),
		"--graceful-timeout", envOr("GUNICORN_GRACEFUL_TIMEOUT", "30"),
		"--keep-alive", "5",
		"--limit-request-line", "8190",
		"--limit-request-fields", "100",
		"--limit-request-field_size", "8190",
		"--max-requests", "10000",
		"--max-requests-jitter", "1000",
		"nominatim_api.server.falcon.server:run_wsgi()",
	}

	// The API only reads, so it runs as the read-only web role: a flaw in the
	// request path cannot write to, or escalate on, the database.
	api := r.WithEnv(
		"NOMINATIM_DATABASE_DSN="+c.DSN(c.WebUser, c.WebUserPassword),
		"NOMINATIM_QUERY_TIMEOUT="+envOr("NOMINATIM_QUERY_TIMEOUT", "10"),
		"NOMINATIM_REQUEST_TIMEOUT="+envOr("NOMINATIM_REQUEST_TIMEOUT", "60"),
	)

	Logf("starting Gunicorn with %d workers on %s as database user %q", c.GunicornWorkers, c.GunicornBind, c.WebUser)
	cmd := api.Command(ctx, "gunicorn", args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting gunicorn: %w", err)
	}
	Logf("--> Nominatim is ready to accept requests")

	// Runner.Command sets Cancel/WaitDelay: cancellation sends SIGTERM and
	// escalates to SIGKILL after the drain deadline.
	err := cmd.Wait()
	stopReplication(replication)

	if ctx.Err() != nil {
		return nil // asked to stop; this is a clean shutdown
	}
	if err != nil {
		return fmt.Errorf("gunicorn exited: %w", err)
	}
	return errors.New("gunicorn exited unexpectedly")
}

func stopReplication(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	Logf("shutting down replication process")
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

// Healthcheck probes the local API in-process, so the image needs no curl.
func Healthcheck(bind string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + bind + "/status.php?format=json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status endpoint returned %s", resp.Status)
	}
	return nil
}
