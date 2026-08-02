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
)

// BaseEnv is the environment handed to every child process.
//
// It is an explicit allow-list plus every NOMINATIM_* and PG* variable present
// in the container environment. The shell version used `sudo -E`, so operators
// could tune any Nominatim setting (NOMINATIM_SEARCH_*, NOMINATIM_LOG_DB, ...)
// or supply libpq TLS material (PGSSLROOTCERT) by passing it to the container;
// a bare allow-list would have silently dropped all of that.
func BaseEnv(c *Config) []string {
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"HOME=" + envOr("NOMINATIM_HOME", "/var/lib/nominatim"),
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PYTHONUNBUFFERED=1",
	}
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		// Passed through, except the DSN, which this process owns.
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

// PrepareProjectDir creates the project directory and renders the Nominatim
// configuration into it.
func PrepareProjectDir(c *Config, uid, gid int) error {
	if err := os.MkdirAll(c.ProjectDir, 0o755); err != nil {
		return err
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

	if err := ensureImported(ctx, c, r); err != nil {
		return err
	}

	appURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	if err := WaitForDatabase(ctx, appURL, 150, 2*time.Second); err != nil {
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
//
// The decision is made only after the server is known to be reachable. Reading
// it from a failed connection would treat a database that is merely still
// booting as an empty one, and take the import branch on a routine restart.
func ensureImported(ctx context.Context, c *Config, r *Runner) error {
	haveAdmin := c.AdminPassword != ""

	// Reach the server first, using whichever credentials exist.
	probeURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	if haveAdmin {
		probeURL = c.LibpqURL(c.AdminUser, c.AdminPassword, "postgres")
	}
	Logf("waiting for PostgreSQL at %s:%d", c.PostgresHost, c.PostgresPort)
	if err := WaitForDatabase(ctx, probeURL, 150, 2*time.Second); err != nil {
		if haveAdmin {
			return err
		}
		return fmt.Errorf("%w\n(no POSTGRES_ADMIN_PASSWORD is set, so the database cannot be provisioned either)", err)
	}

	targetURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	if haveAdmin {
		targetURL = c.LibpqURL(c.AdminUser, c.AdminPassword, c.PostgresDB)
	}

	complete, err := ImportComplete(ctx, targetURL, c.PostgresDB)
	if err != nil {
		return err
	}
	if complete {
		Logf("existing Nominatim import found in %q — skipping import", c.PostgresDB)
		return chownProjectFiles(c, r.UID, r.GID)
	}

	hasData, err := HasNominatimData(ctx, targetURL)
	if err != nil {
		return err
	}
	if hasData && !c.AllowDropExistingDB {
		// Either a database imported by an older release of this image, or an
		// import that died part-way through. Nominatim's own validator is the
		// authority on which — the previous file marker could not tell them
		// apart, and an unfinished import would be served as if it were done.
		Logf("database %q holds Nominatim tables but no completion marker; validating", c.PostgresDB)
		if err := r.Run(ctx, "nominatim", "admin", "--check-database", "--project-dir", c.ProjectDir); err != nil {
			return fmt.Errorf("database %q contains an incomplete or invalid Nominatim schema: %w\n"+
				"Set ALLOW_DROP_EXISTING_DB=true to discard it and import again", c.PostgresDB, err)
		}
		Logf("validation passed; adopting the existing import")
		if err := MarkImportComplete(ctx, targetURL, c.PostgresDB); err != nil {
			return err
		}
		return chownProjectFiles(c, r.UID, r.GID)
	}

	Logf("no completed Nominatim import in %q — running import", c.PostgresDB)
	if err := RunImport(ctx, c, r); err != nil {
		return err
	}
	// Written only after the import has fully succeeded, so an interrupted run
	// is retried rather than served.
	return MarkImportComplete(ctx, c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB), c.PostgresDB)
}

// startReplication launches the background diff process, if configured.
func startReplication(ctx context.Context, c *Config, r *Runner) (*exec.Cmd, error) {
	if c.ReplicationURL == "" || c.Freeze {
		Logf("skipping replication")
		return nil, nil
	}
	dl := NewDownloader(c.UserAgent)
	if !dl.Reachable(ctx, c.ReplicationURL, 3, 2*time.Second) {
		Logf("WARNING: REPLICATION_URL unreachable; skipping replication")
		return nil, nil
	}
	// Re-init on every start in case the replication settings changed. This also
	// leaves the state usable by a manual `nominatim replication --once` even
	// when no UPDATE_MODE is configured, which is what the shell version did.
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
	// Reaped here rather than left to accumulate: with UPDATE_MODE=once the
	// process exits early, and nothing else would ever wait on it.
	go func() { _ = cmd.Wait() }()
	return cmd, nil
}

// runGunicorn starts the API in the foreground and supervises it.
//
// The shell version used --daemon plus a PID-file poll. That returned success
// before the socket was bound (so a startup failure hung the container
// forever), added up to five seconds of shutdown latency, could read a stale
// PID left in /tmp by a previous run, and finished with an unconditional
// `exit 0` that reported a crashed API as a clean exit to the orchestrator.
func runGunicorn(ctx context.Context, c *Config, r *Runner, replication *exec.Cmd) error {
	args := []string{
		"--bind", c.GunicornBind,
		"--workers", fmt.Sprint(c.GunicornWorkers),
		"--worker-class", "asgi",
		"--worker-tmp-dir", envOr("GUNICORN_WORKER_TMP_DIR", "/dev/shm"),
		"--access-logfile", "-",
		"--error-logfile", "-",
		"--enable-stdio-inheritance",
		// Bounded request handling: without these a single slow or oversized
		// client can hold a worker indefinitely, and the process never recycles.
		"--timeout", envOr("GUNICORN_TIMEOUT", "60"),
		"--graceful-timeout", envOr("GUNICORN_GRACEFUL_TIMEOUT", "30"),
		"--keep-alive", "5",
		"--limit-request-line", "8190",
		"--limit-request-fields", envOr("GUNICORN_LIMIT_REQUEST_FIELDS", "100"),
		"--limit-request-field_size", "8190",
		"--max-requests", envOr("GUNICORN_MAX_REQUESTS", "10000"),
		"--max-requests-jitter", "1000",
		"nominatim_api.server.falcon.server:run_wsgi()",
	}

	// The API only ever reads. Giving it the read-only web role means a flaw in
	// the request path cannot write to, or escalate on, the database.
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

	// Runner.Command sets Cancel/WaitDelay, so cancelling the context sends
	// SIGTERM and escalates to SIGKILL if the drain deadline passes.
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

// Healthcheck probes the local API. Implemented in-process so the image needs
// no curl for its HEALTHCHECK.
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
