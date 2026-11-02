package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const nominatimHome = "/var/lib/nominatim"

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
		switch k {
		case "NOMINATIM_DATABASE_DSN", "NOMINATIM_PASSWORD", "NOMINATIM_WEBUSER_PASSWORD":
			continue
		// Never ALL_PROXY: Go's downloader ignores it, and pyosmium must use the same proxy.
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
			"http_proxy", "https_proxy", "no_proxy":
			env = append(env, kv)
			continue
		}
		if strings.HasPrefix(k, "NOMINATIM_") || strings.HasPrefix(k, "PG") {
			env = append(env, kv)
		}
	}
	return append(env, "NOMINATIM_DATABASE_DSN="+c.DSN("nominatim", c.NominatimPassword))
}

func NewRunner(c *Config) (*Runner, error) {
	uid, gid, err := LookupNominatimUser()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.ProjectDir, 0o755); err != nil {
		return nil, err
	}
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

func Serve(ctx context.Context, c *Config) error {
	r, err := NewRunner(c)
	if err != nil {
		return err
	}
	if c.Debug {
		// Withhold the DSN line: RegisterSecret ignores secrets shorter than four characters.
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

	if haveAdmin {
		unlock, err := lockImport(ctx, probeURL, c.PostgresDB)
		if err != nil {
			return err
		}
		defer unlock()
	}

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

	hasData, err := databaseHasTables(ictx, targetURL)
	if err != nil {
		if !c.AllowDropExistingDB {
			return fmt.Errorf("cannot inspect database %q, refusing to import over it: %w\n"+
				"Set ALLOW_DROP_EXISTING_DB=true (or run `nominatim-ctl reimport`) to discard it and import again", c.PostgresDB, err)
		}
		Logf("cannot inspect database %q (%v); validating it", c.PostgresDB, err)
		hasData = true
	}

	if hasData {
		Logf("database %q holds tables but no completion marker; validating", c.PostgresDB)
		if haveAdmin {
			if err := reconcileRoles(ctx, c, probeURL); err != nil {
				Logf("WARNING: role passwords not reconciled: %v", err)
			}
		}
		appConn, err := pgx.Connect(ictx, c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB))
		if err != nil {
			return fmt.Errorf("cannot validate database %q: the nominatim role cannot connect to it: %w\n"+
				"Nothing was dropped. Check NOMINATIM_PASSWORD, the role's CONNECT privilege on the database and pg_hba.conf", c.PostgresDB, err)
		}
		appConn.Close(ctx)
		err = r.Run(ctx, "nominatim", "admin", "--check-database", "--project-dir", c.ProjectDir)
		if err != nil && (!c.AllowDropExistingDB || ctx.Err() != nil) {
			return fmt.Errorf("database %q contains an incomplete or invalid Nominatim schema: %w\n"+
				"Set ALLOW_DROP_EXISTING_DB=true to discard it and import again", c.PostgresDB, err)
		}
		if err != nil {
			Logf("WARNING: validation failed; ALLOW_DROP_EXISTING_DB=true, discarding %q and importing again", c.PostgresDB)
			return importChecked(ctx, c, r)
		}
		Logf("validation passed; adopting the existing import")
		if err := configureReplicationOrFreeze(ctx, c, r, NewDownloader(c.UserAgent)); err != nil {
			Logf("WARNING: adopted import: %v (the nominatim role may not own its tables)", err)
		}
		if err := finishImport(ctx, c, targetURL); err != nil {
			Logf("WARNING: adopted import not stamped: %v. It is served and validated again on the next start; "+
				"to stamp it, run as its owner: COMMENT ON DATABASE %s IS %s",
				err, QuoteIdentifier(c.PostgresDB), QuoteLiteral(importMarker))
		}
		return chownProjectFiles(c, r.UID, r.GID)
	}

	Logf("no completed Nominatim import in %q, running import", c.PostgresDB)
	return importChecked(ctx, c, r)
}

func importChecked(ctx context.Context, c *Config, r *Runner) error {
	if err := checkImportable(c); err != nil {
		return err
	}
	return runImport(ctx, c, r)
}

const replicationRetry = time.Minute

func startReplication(ctx context.Context, c *Config, r *Runner, frozen bool) (stop func(), done <-chan error, err error) {
	if c.ReplicationURL == "" {
		Logf("skipping replication")
		return nil, nil, nil
	}
	if frozen {
		Logf("WARNING: %q is frozen (imported with FREEZE=true) and cannot take updates; serving without replication. Re-import without FREEZE to enable them", c.PostgresDB)
		return nil, nil, nil
	}
	if !HaveImportTools() {
		if c.UpdateMode != "" {
			return nil, nil, fmt.Errorf("UPDATE_MODE=%q needs osm2pgsql, which the serve-only image does not ship; run replication from the full image", c.UpdateMode)
		}
		Logf("serve-only image: skipping replication (no osm2pgsql)")
		return nil, nil, nil
	}
	dl := NewDownloader(c.UserAgent)
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
	Logf("WARNING: REPLICATION_URL unreachable; serving without replication and retrying every %v", replicationRetry)
	go func() {
		for {
			select {
			case <-rctx.Done():
				// Without this send, runGunicorn's shutdown wait runs out Runner.Grace and docker stop kills the container.
				ch <- rctx.Err()
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

func replicationArgs(c *Config) []string {
	args := []string{"replication", "--project-dir", c.ProjectDir, "--threads", fmt.Sprint(c.Threads)}
	switch c.UpdateMode {
	case "once":
		args = append(args, "--once")
	case "catch-up":
		args = append(args, "--catch-up")
	}
	return args
}

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

	if !NewDownloader(c.UserAgent).Reachable(ctx, c.ReplicationURL, 3, 2*time.Second) {
		return fmt.Errorf("REPLICATION_URL %s is unreachable", c.ReplicationURL)
	}
	if err := r.Run(ctx, "nominatim", "replication", "--init", "--project-dir", c.ProjectDir); err != nil {
		return err
	}
	Logf("starting replication (%s)", c.UpdateMode)
	return r.Run(ctx, "nominatim", replicationArgs(c)...)
}

func runGunicorn(ctx context.Context, c *Config, r *Runner, stopReplication func(), replicationDone <-chan error) error {
	args := []string{
		"--bind", c.GunicornBind,
		"--workers", fmt.Sprint(c.GunicornWorkers),
		"--worker-class", "asgi",
		"--worker-tmp-dir", "/dev/shm",
		"--access-logfile", "-",
		"--error-logfile", "-",
		"--enable-stdio-inheritance",
		"--timeout", fmt.Sprint(c.GunicornTimeout),
		"--graceful-timeout", fmt.Sprint(c.GunicornGracefulTimeout),
		"--keep-alive", "5",
		"--limit-request-line", "8190",
		"--limit-request-fields", "100",
		"--limit-request-field_size", "8190",
		"--max-requests", "10000",
		"--max-requests-jitter", "1000",
		"nominatim_api.server.falcon.server:run_wsgi()",
	}

	api := r.WithEnv(
		"NOMINATIM_DATABASE_DSN="+c.DSN(c.WebUser, c.WebUserPassword),
		"NOMINATIM_QUERY_TIMEOUT="+envOr("NOMINATIM_QUERY_TIMEOUT", "10"),
		"NOMINATIM_REQUEST_TIMEOUT="+envOr("NOMINATIM_REQUEST_TIMEOUT", "60"),
	)

	Logf("starting Gunicorn with %d workers on %s as database user %q", c.GunicornWorkers, c.GunicornBind, c.WebUser)
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
			replicationDone = nil
			if ctx.Err() != nil {
				continue
			}
			status := "status 0"
			if rerr != nil {
				status = rerr.Error()
			}
			if c.UpdateMode != "continuous" {
				Logf("replication (%s) finished: %s", c.UpdateMode, status)
				continue
			}
			Logf("continuous replication exited (%s); stopping the API", status)
			stopGunicorn()
			<-gunicornDone
			return fmt.Errorf("continuous replication exited: %s", status)
		}
	}
	if stopReplication != nil {
		Logf("shutting down replication process")
		stopReplication()
		if replicationDone != nil {
			select {
			case <-replicationDone:
			case <-time.After(r.Grace + time.Second):
			}
		}
	}

	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gunicorn exited: %w", err)
	}
	return errors.New("gunicorn exited unexpectedly")
}

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
	var st struct {
		Status  *int   `json:"status"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st); err != nil {
		return fmt.Errorf("status endpoint returned no JSON status: %w", err)
	}
	if st.Status == nil || *st.Status != 0 {
		return fmt.Errorf("status endpoint reports failure: %s", st.Message)
	}
	return nil
}
