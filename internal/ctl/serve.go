package ctl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
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
		// The DSN is owned by this process; the role passwords must not reach a
		// child (Gunicorn connects as the web role).
		switch k {
		case "NOMINATIM_DATABASE_DSN", "NOMINATIM_PASSWORD", "NOMINATIM_WEBUSER_PASSWORD":
			continue
		// The same network the downloader uses: pyosmium fetches the diffs.
		// ALL_PROXY is left out: Go ignores it, and the two must agree.
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
			"http_proxy", "https_proxy", "no_proxy":
			env = append(env, kv)
			continue
		}
		if strings.HasPrefix(k, "NOMINATIM_") || strings.HasPrefix(k, "PG") {
			env = append(env, kv)
		}
	}
	// Appended last so it wins over anything inherited above.
	return append(env, "NOMINATIM_DATABASE_DSN="+c.DSN("nominatim", c.NominatimPassword))
}

// NewRunner resolves the unprivileged account, prepares the project directory
// and returns the Runner every subcommand launches Nominatim through.
func NewRunner(c *Config) (*Runner, error) {
	uid, gid, err := LookupNominatimUser()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.ProjectDir, 0o755); err != nil {
		return nil, err
	}
	// Under a read-only root filesystem, $HOME is a tmpfs mounted fresh and
	// root-owned on every boot.
	if os.Geteuid() == 0 {
		if err := os.Chown(nominatimHome, uid, gid); err != nil {
			return nil, fmt.Errorf("chown %s: %w", nominatimHome, err)
		}
	}
	if err := WriteEnvFile(c, uid, gid); err != nil {
		return nil, err
	}
	return &Runner{UID: uid, GID: gid, Dir: c.ProjectDir, Env: BaseEnv(c), Grace: c.ShutdownGrace()}, nil
}

// Serve runs the full container lifecycle: configure, import if needed, then
// supervise the API server until it exits or the context is cancelled.
func Serve(ctx context.Context, c *Config) error {
	r, err := NewRunner(c)
	if err != nil {
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

	if err := EnsureImported(ctx, c, r); err != nil {
		return err
	}

	appURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	if err := waitForDatabase(ctx, appURL, 150, 2*time.Second); err != nil {
		return err
	}
	frozen, owned, err := databaseState(ctx, appURL, c.PostgresDB)
	if err != nil {
		return err
	}

	// An adopted database owned by another role cannot have its functions
	// replaced; they exist in a validated database, so there it is a warning.
	// On a database this image owns, a failure is real and stops the start.
	if err := r.Run(ctx, "nominatim", "refresh", "--functions", "--project-dir", c.ProjectDir); err != nil {
		if owned || ctx.Err() != nil {
			return err
		}
		Logf("WARNING: SQL functions not refreshed: %v (the database is not owned by the nominatim role)", err)
	}

	replication, replicationDone, err := startReplication(ctx, c, r, frozen)
	if err != nil {
		return err
	}

	if c.WarmupOnStartup {
		Logf("warming database caches")
		if err := warmCaches(ctx, c, r); err != nil {
			return err
		}
		Logf("Warming finished")
	} else {
		Logf("Skipping cache warmup")
	}

	return runGunicorn(ctx, c, r, replication, replicationDone)
}

// EnsureImported decides whether an import is required, and runs one if so.
// The decision is made only after the server is known reachable, because a booting
// database must not be mistaken for an empty one on a routine restart.
func EnsureImported(ctx context.Context, c *Config, r *Runner) error {
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

	// settled finishes for a database confirmed ours. Roles are server-wide, so
	// only then are they reconciled; failure is a warning (adopted roles lack
	// our marker).
	settled := func() error {
		if haveAdmin {
			if err := reconcileRoles(ctx, c, probeURL); err != nil {
				Logf("WARNING: role passwords not reconciled: %v", err)
			}
		}
		return chownProjectFiles(c, r.UID, r.GID)
	}

	targetURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	if haveAdmin {
		targetURL = c.LibpqURL(adminUser, c.AdminPassword, c.PostgresDB)
	}

	// Over the probe connection, already known to work: the marker lives in
	// pg_database, so this holds where pg_hba keeps the admin out of POSTGRES_DB.
	// Bounded: a server that accepts TCP but stalls must end in an error,
	// not a hang, and an error here never leads to a drop.
	ictx, cancelInspect := context.WithTimeout(ctx, 30*time.Second)
	defer cancelInspect()
	complete, err := readImportMarker(ictx, probeURL, c.PostgresDB)
	if err != nil {
		return fmt.Errorf("cannot read the import marker of %q: %w", c.PostgresDB, err)
	}
	if complete {
		Logf("existing Nominatim import found in %q, skipping import", c.PostgresDB)
		return settled()
	}

	// The fallback is an import that drops the database, so only a database
	// proven missing or empty may reach it; any other error stops here.
	hasData, err := databaseHasTables(ictx, targetURL)
	if err != nil {
		if !c.AllowDropExistingDB {
			return fmt.Errorf("cannot inspect database %q, refusing to import over it: %w\n"+
				"Set ALLOW_DROP_EXISTING_DB=true (or run `nominatim-ctl reimport`) to discard it and import again", c.PostgresDB, err)
		}
		// The drop is allowed either way; the import below is the way out
		// for a database left invalid by an interrupted DROP.
		Logf("cannot inspect database %q (%v); ALLOW_DROP_EXISTING_DB=true, importing over it", c.PostgresDB, err)
		hasData = true
	}

	if hasData && !c.AllowDropExistingDB {
		// Tables without a completion marker: an import that died part-way, a
		// database imported elsewhere, or not Nominatim at all. Nominatim's
		// validator decides which.
		Logf("database %q holds tables but no completion marker; validating", c.PostgresDB)
		if err := r.Run(ctx, "nominatim", "admin", "--check-database", "--project-dir", c.ProjectDir); err != nil {
			return fmt.Errorf("database %q contains an incomplete or invalid Nominatim schema: %w\n"+
				"Set ALLOW_DROP_EXISTING_DB=true to discard it and import again", c.PostgresDB, err)
		}
		Logf("validation passed; adopting the existing import")
		// The steps a fresh import runs after its own check. The adopted
		// database was not created here, so these may lack ownership; the
		// import is valid either way, so a failure is a warning, not an outage.
		if err := configureReplicationOrFreeze(ctx, c, r, NewDownloader(c.UserAgent)); err != nil {
			Logf("WARNING: adopted import: %v (the nominatim role may not own its tables)", err)
		}
		if err := finishImport(ctx, c, targetURL); err != nil {
			Logf("WARNING: adopted import not stamped: %v. It is served and validated again on the next start; "+
				"to stamp it, run as its owner: COMMENT ON DATABASE %s IS %s",
				err, QuoteIdentifier(c.PostgresDB), QuoteLiteral(importMarker))
		}
		return settled()
	}

	Logf("no completed Nominatim import in %q, running import", c.PostgresDB)
	return RunImport(ctx, c, r)
}

// replicationRetry is how often serve re-checks an unreachable REPLICATION_URL.
const replicationRetry = time.Minute

// startReplication launches the background diff process, if configured. The
// channel receives its exit status; stop terminates it. REPLICATION_URL wins
// over FREEZE, as in the import, which then never froze the database.
func startReplication(ctx context.Context, c *Config, r *Runner, frozen bool) (stop func(), done <-chan error, err error) {
	if c.ReplicationURL == "" {
		Logf("skipping replication")
		return nil, nil, nil
	}
	// The state decides, not FREEZE: a frozen database has no update tables,
	// and replicating into it would fail on every diff.
	if frozen {
		Logf("WARNING: %q is frozen (imported with FREEZE=true) and cannot take updates; serving without replication. Re-import without FREEZE to enable them", c.PostgresDB)
		return nil, nil, nil
	}
	// nominatim replication shells out to osm2pgsql for every diff, which the
	// serve-only image does not ship. An explicit UPDATE_MODE is a promise
	// this image cannot keep, so it fails rather than serving stale data.
	if !HaveImportTools() {
		if c.UpdateMode != "" {
			return nil, nil, fmt.Errorf("UPDATE_MODE=%q needs osm2pgsql, which the serve-only image does not ship; run replication from the full image", c.UpdateMode)
		}
		Logf("serve-only image: skipping replication (no osm2pgsql)")
		return nil, nil, nil
	}
	dl := NewDownloader(c.UserAgent)
	// Re-init on every start in case the replication settings changed; this
	// also keeps the state usable for a manual `nominatim replication --once`.
	if c.UpdateMode == "" {
		if !dl.Reachable(ctx, c.ReplicationURL, 3, 2*time.Second) {
			Logf("WARNING: REPLICATION_URL unreachable; skipping replication")
			return nil, nil, nil
		}
		if err := r.Run(ctx, "nominatim", "replication", "--init", "--project-dir", c.ProjectDir); err != nil {
			return nil, nil, err
		}
		Logf("no UPDATE_MODE set; not starting a background replication process")
		return nil, nil, nil
	}

	rctx, cancel := context.WithCancel(ctx)
	ch := make(chan error, 1)
	launch := func() error {
		if err := r.Run(rctx, "nominatim", "replication", "--init", "--project-dir", c.ProjectDir); err != nil {
			return err
		}
		Logf("starting replication (%s)", c.UpdateMode)
		cmd := r.Command(rctx, "nominatim", replicationArgs(c)...)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("starting replication: %w", err)
		}
		// Reaped here, and the status handed to runGunicorn, which supervises it.
		go func() { ch <- cmd.Wait() }()
		return nil
	}
	if dl.Reachable(rctx, c.ReplicationURL, 3, 2*time.Second) {
		if err := launch(); err != nil {
			cancel()
			return nil, nil, err
		}
		return cancel, ch, nil
	}
	// Serving beats freshness: an upstream outage must not take the API down.
	// Replication starts, and is supervised, once the URL answers again.
	Logf("WARNING: REPLICATION_URL unreachable; serving without replication and retrying every %v", replicationRetry)
	go func() {
		for {
			select {
			case <-rctx.Done():
				ch <- rctx.Err() // lets runGunicorn's shutdown wait finish
				return
			case <-time.After(replicationRetry):
			}
			if dl.Reachable(rctx, c.ReplicationURL, 1, 0) {
				if err := launch(); err != nil {
					ch <- err
				}
				return
			}
		}
	}()
	return cancel, ch, nil
}

// replicationArgs builds the `nominatim replication` invocation for UpdateMode.
func replicationArgs(c *Config) []string {
	// --threads bounds the indexing connections, as for the import.
	args := []string{"replication", "--project-dir", c.ProjectDir, "--threads", fmt.Sprint(c.Threads)}
	switch c.UpdateMode {
	case "once":
		args = append(args, "--once")
	case "catch-up":
		args = append(args, "--catch-up")
	}
	return args
}

// Replicate runs replication in the foreground: the updater container of a
// split deployment. `once` and `catch-up` exit when done; the restart policy
// governs `continuous`. FREEZE is ignored: with REPLICATION_URL set, the
// import never froze the database.
func Replicate(ctx context.Context, c *Config) error {
	switch {
	case c.ReplicationURL == "":
		return errors.New("REPLICATION_URL must be set to replicate")
	case !HaveImportTools():
		return errors.New("this is the serve-only image: osm2pgsql is not installed, so it cannot apply updates; run replicate from the full image")
	}
	if c.UpdateMode == "" {
		c.UpdateMode = "continuous"
	}

	r, err := NewRunner(c)
	if err != nil {
		return err
	}

	// POSTGRES_DB itself, as serve does: the role may be kept out of the
	// maintenance database. A missing one ends the wait at once.
	probeURL := c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)
	Logf("waiting for PostgreSQL at %s:%d", c.PostgresHost, c.PostgresPort)
	if err := waitForDatabase(ctx, probeURL, 150, 2*time.Second); err != nil {
		if isMissingDatabase(err) {
			return fmt.Errorf("no completed Nominatim import in %q (the database does not exist); run `nominatim-ctl import` first", c.PostgresDB)
		}
		return err
	}
	complete, err := readImportMarker(ctx, probeURL, c.PostgresDB)
	if err != nil {
		return fmt.Errorf("cannot read the import marker of %q: %w", c.PostgresDB, err)
	}
	if !complete {
		return fmt.Errorf("no completed Nominatim import in %q; run `nominatim-ctl import` first", c.PostgresDB)
	}
	if frozen, _, err := databaseState(ctx, probeURL, c.PostgresDB); err != nil {
		return err
	} else if frozen {
		return fmt.Errorf("%q is frozen (imported with FREEZE=true) and cannot take updates; re-import without FREEZE", c.PostgresDB)
	}

	// Unlike serve, unreachable is an error: exit non-zero, let the restart policy retry.
	if !NewDownloader(c.UserAgent).Reachable(ctx, c.ReplicationURL, 3, 2*time.Second) {
		return fmt.Errorf("REPLICATION_URL %s is unreachable", c.ReplicationURL)
	}
	if err := r.Run(ctx, "nominatim", "replication", "--init", "--project-dir", c.ProjectDir); err != nil {
		return err
	}
	Logf("starting replication (%s)", c.UpdateMode)
	return r.Run(ctx, "nominatim", replicationArgs(c)...)
}

// runGunicorn starts the API in the foreground and supervises it, so a crash
// exits non-zero while a signalled stop exits clean.
func runGunicorn(ctx context.Context, c *Config, r *Runner, stopReplication func(), replicationDone <-chan error) error {
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
		"--graceful-timeout", fmt.Sprint(c.GunicornGracefulTimeout),
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
	// Runner.Command sets Cancel/WaitDelay: cancellation sends SIGTERM and
	// escalates to SIGKILL after the drain deadline.
	gctx, stopGunicorn := context.WithCancel(ctx)
	defer stopGunicorn()
	cmd := api.Command(gctx, "gunicorn", args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting gunicorn: %w", err)
	}
	Logf("--> Nominatim is ready to accept requests")
	gunicornDone := make(chan error, 1)
	go func() { gunicornDone <- cmd.Wait() }()

	var err error
	for waiting := true; waiting; {
		select {
		case err = <-gunicornDone:
			waiting = false
		case rerr := <-replicationDone:
			replicationDone = nil // a nil channel never fires again
			if ctx.Err() != nil {
				continue
			}
			status := "status 0"
			if rerr != nil {
				status = rerr.Error() // leaves via Logf, or Errf in main
			}
			if c.UpdateMode != "continuous" {
				Logf("replication (%s) finished: %s", c.UpdateMode, status)
				continue
			}
			// Continuous replication is meant to run for good; serving on
			// without it would let the data go stale unnoticed. Exit so the
			// restart policy brings both back.
			Logf("continuous replication exited (%s); stopping the API", status)
			stopGunicorn()
			<-gunicornDone
			return fmt.Errorf("continuous replication exited: %s", status)
		}
	}
	if stopReplication != nil {
		Logf("shutting down replication process")
		stopReplication()
		// Let SIGTERM finish the current diff; WaitDelay bounds it.
		if replicationDone != nil {
			select {
			case <-replicationDone:
			case <-time.After(r.grace() + time.Second):
			}
		}
	}

	if ctx.Err() != nil {
		return nil // asked to stop; this is a clean shutdown
	}
	if err != nil {
		return fmt.Errorf("gunicorn exited: %w", err)
	}
	return errors.New("gunicorn exited unexpectedly")
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
