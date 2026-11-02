package ctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunImport imports unconditionally (reimport); EnsureImported decides first.
// It waits while another container imports into the same database.
func RunImport(ctx context.Context, c *Config, r *Runner) error {
	if err := checkImportable(c); err != nil {
		return err
	}
	adminURL := c.LibpqURL(adminUser, c.AdminPassword, "postgres")
	Logf("waiting for PostgreSQL at %s:%d", c.PostgresHost, c.PostgresPort)
	if err := waitForDatabase(ctx, adminURL, 150, 2*time.Second); err != nil {
		return err
	}
	unlock, err := lockImport(ctx, adminURL, c.PostgresDB)
	if err != nil {
		return err
	}
	defer unlock()
	return runImport(ctx, c, r)
}

// checkImportable refuses before any download or provisioning happens.
func checkImportable(c *Config) error {
	if err := c.ValidateForImport(); err != nil {
		return err
	}
	if !HaveImportTools() {
		return errors.New("this is the serve-only image: osm2pgsql is not installed, so it cannot run an import.\n" +
			"Run the import once with the full (non -serve) image against the same database, then start this one again")
	}
	return nil
}

// runImport performs the one-time database provisioning and OSM import. The
// caller has run checkImportable and holds the import lock.
func runImport(ctx context.Context, c *Config, r *Runner) error {
	dl := NewDownloader(c.UserAgent)

	if err := fetchDatasets(ctx, c, dl); err != nil {
		return err
	}
	if c.PBFURL != "" {
		Logf("downloading OSM extract from %s", c.PBFURL)
		if err := dl.Fetch(ctx, c.PBFURL, c.OSMFile(), os.Getenv("PBF_SHA256")); err != nil {
			return err
		}
	} else {
		Logf("reading OSM extract from %s", c.PBFPath)
	}

	if err := provisionDatabase(ctx, c); err != nil {
		return err
	}

	if err := chownProjectFiles(c, r.UID, r.GID); err != nil {
		return err
	}

	importArgs := []string{"import", "--osm-file", c.OSMFile(), "--threads", fmt.Sprint(c.Threads), "--project-dir", c.ProjectDir}
	if c.ReverseOnly {
		importArgs = append(importArgs, "--reverse-only")
	}
	if err := r.Run(ctx, "nominatim", importArgs...); err != nil {
		return err
	}

	if c.TigerEnabled() {
		tiger := filepath.Join(c.ProjectDir, "tiger-nominatim-preprocessed.csv.tar.gz")
		Logf("importing Tiger address data")
		if err := r.Run(ctx, "nominatim", "add-data", "--tiger-data", tiger, "--project-dir", c.ProjectDir); err != nil {
			return err
		}
	}

	// The import can leave parent places flagged for indexing, which would
	// make --check-database report unindexed entries.
	if err := r.Run(ctx, "nominatim", "index", "--threads", fmt.Sprint(c.Threads), "--project-dir", c.ProjectDir); err != nil {
		return err
	}
	if err := r.Run(ctx, "nominatim", "admin", "--check-database", "--project-dir", c.ProjectDir); err != nil {
		return err
	}

	if err := configureReplicationOrFreeze(ctx, c, r, dl); err != nil {
		return err
	}

	if err := warmCaches(ctx, c, r); err != nil {
		return err
	}
	return finishImport(ctx, c, c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB))
}

// finishImport gathers planner statistics, records completion and removes the
// downloads; shared by a fresh import and an adopted one. COMMENT ON DATABASE
// needs ownership, so the caller chooses which role url connects as.
func finishImport(ctx context.Context, c *Config, url string) error {
	Logf("gathering planner statistics")
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "ANALYZE"); err != nil {
		return err
	}
	// Recorded only after full success. An interrupted run leaves tables but
	// no marker, so the next start validates them before anything else.
	if err := markImportComplete(ctx, conn, c.PostgresDB); err != nil {
		return err
	}

	cleanupDownloads(c)
	return nil
}

// fetchDatasets resolves the optional supplementary datasets. Nominatim's
// import reads them from the project directory by name, so a disabled one
// must not leave a file of ours there.
func fetchDatasets(ctx context.Context, c *Config, dl *Downloader) error {
	for _, d := range Datasets {
		v := c.DatasetValues[d.EnvVar]
		dest := filepath.Join(c.ProjectDir, d.Local)
		switch {
		case v == "true":
			// An operator file there: Fetch would truncate it, and a checksum
			// mismatch delete it.
			if isOperatorFile(dest) {
				return operatorFileError(d, dest)
			}
			url := c.MirrorBaseURL + "/" + d.Remote
			Logf("downloading %s from %s", d.Label, url)
			if err := dl.Fetch(ctx, url, dest, os.Getenv(d.EnvVar+"_SHA256")); err != nil {
				return fmt.Errorf("%s: %w", d.Label, err)
			}
		case v != "" && v != "false":
			// Absolute-path form, already validated. The operator's own file at
			// dest is used in place: replacing it would delete it.
			if isSameRegularFile(v, dest) {
				Logf("using %s from %s", d.Label, v)
				_ = os.Remove(statePath(dest)) // the operator's now: never cleaned up
				continue
			}
			Logf("linking %s from %s", d.Label, v)
			kept, err := removeLeftover(dest)
			if err != nil {
				return fmt.Errorf("%s: %w", d.Label, err)
			}
			if kept {
				return operatorFileError(d, dest)
			}
			if err := os.Symlink(v, dest); err != nil {
				return fmt.Errorf("%s: %w", d.Label, err)
			}
		default:
			kept, err := removeLeftover(dest)
			if err != nil {
				return fmt.Errorf("%s: %w", d.Label, err)
			}
			// Tiger is the exception: only runImport's add-data reads it.
			if kept && d.EnvVar != "IMPORT_TIGER_ADDRESSES" {
				Logf("WARNING: %s is not set, but %s exists; Nominatim imports it anyway", d.EnvVar, dest)
				continue
			}
			Logf("skipping optional %s import", d.Label)
		}
	}
	return nil
}

// isSameRegularFile reports whether dest is a regular file (not a symlink of
// ours) that path names too.
func isSameRegularFile(path, dest string) bool {
	pi, err := os.Stat(path)
	if err != nil {
		return false
	}
	di, err := os.Lstat(dest)
	return err == nil && di.Mode().IsRegular() && os.SameFile(pi, di)
}

// isOperatorFile reports whether path exists and is not ours: neither a
// symlink nor a download with its state file.
func isOperatorFile(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	_, err = os.Lstat(statePath(path))
	return err != nil
}

func operatorFileError(d Dataset, dest string) error {
	return fmt.Errorf("%s: %s already holds a file this image did not create; move it away, or set %s=%s to use it in place",
		d.Label, dest, d.EnvVar, dest)
}

// removeLeftover deletes what an earlier run left at path: a symlink, or a
// download with its state file. An operator file is kept, which it reports.
func removeLeftover(path string) (kept bool, err error) {
	if isOperatorFile(path) {
		return true, nil
	}
	for _, p := range []string{path, statePath(path)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// provisionDatabase creates the roles and clears any stale database using the
// administrative credentials. The application role is not a superuser.
func provisionDatabase(ctx context.Context, c *Config) error {
	adminURL := c.LibpqURL(adminUser, c.AdminPassword, "postgres")
	Logf("waiting for PostgreSQL at %s:%d", c.PostgresHost, c.PostgresPort)
	if err := waitForDatabase(ctx, adminURL, 150, 2*time.Second); err != nil {
		return err
	}

	// Bounded probe: a server that accepts TCP but stalls the handshake must
	// not block here without a diagnostic.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	defer cancelProbe()
	hasData, err := databaseHasTables(probeCtx, c.LibpqURL(adminUser, c.AdminPassword, c.PostgresDB))
	if err != nil {
		if !c.AllowDropExistingDB {
			return fmt.Errorf("cannot inspect database %q, refusing to drop it: %w", c.PostgresDB, err)
		}
		// The drop is allowed either way, and this is the only way out for a
		// database left invalid by an interrupted DROP (PostgreSQL 16+).
		Logf("cannot inspect database %q (%v); treating it as populated", c.PostgresDB, err)
		hasData = true
	}

	if err := reconcileRoles(ctx, c, adminURL); err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	if err := dropDatabase(ctx, conn, c.PostgresDB, hasData, c.AllowDropExistingDB); err != nil {
		return err
	}

	// A superuser role installs its own extensions; nothing to pre-seed then.
	if c.ProvisionExtensions && !roleIsSuperuser(c.RoleOptions) {
		tmpl, err := pgx.Connect(ctx, c.LibpqURL(adminUser, c.AdminPassword, "template1"))
		if err != nil {
			return err
		}
		err = provisionExtensions(ctx, tmpl)
		// Closed here, not deferred: the import's CREATE DATABASE ... TEMPLATE
		// template1 fails while a session is still attached to template1.
		tmpl.Close(ctx)
		if err != nil {
			return fmt.Errorf("installing PostGIS into template1 (set PROVISION_EXTENSIONS=false if your "+
				"provider manages extensions, or NOMINATIM_ROLE_OPTIONS=SUPERUSER to let the import do it): %w", err)
		}
	}
	return nil
}

// roleIsSuperuser reports whether NOMINATIM_ROLE_OPTIONS grants SUPERUSER. A
// substring test would also match NOSUPERUSER.
func roleIsSuperuser(options string) bool {
	return slices.ContainsFunc(strings.Fields(options), func(o string) bool { return strings.EqualFold(o, "SUPERUSER") })
}

// configureReplicationOrFreeze runs the post-import replication/freeze branch.
func configureReplicationOrFreeze(ctx context.Context, c *Config, r *Runner, dl *Downloader) error {
	if c.ReplicationURL != "" {
		if c.Freeze {
			Logf("skipping freeze because REPLICATION_URL is set")
		}
		// A passing outage must not turn replication off for good: serve and
		// the updater run --init again on every start.
		if !dl.Reachable(ctx, c.ReplicationURL, 3, 2*time.Second) {
			Logf("WARNING: REPLICATION_URL unreachable; skipping replication --init for now")
			return nil
		}
		return r.Run(ctx, "nominatim", "replication", "--init", "--project-dir", c.ProjectDir)
	}
	if c.Freeze {
		Logf("freezing database")
		return r.Run(ctx, "nominatim", "freeze", "--project-dir", c.ProjectDir)
	}
	return nil
}

// warmCaches runs the warm-up pass under relaxed timeouts.
func warmCaches(ctx context.Context, c *Config, r *Runner) error {
	args := []string{"admin", "--warm", "--project-dir", c.ProjectDir}
	if c.ReverseOnly {
		args = append(args, "--reverse")
	}
	warm := r.WithEnv("NOMINATIM_QUERY_TIMEOUT=600", "NOMINATIM_REQUEST_TIMEOUT=3600")
	return warm.Run(ctx, "nominatim", args...)
}

// cleanupDownloads removes exactly the files the import created, never a
// glob, and never a dataset file the operator placed in the project directory.
func cleanupDownloads(c *Config) {
	Logf("removing downloaded dumps in %s", c.ProjectDir)
	for _, d := range Datasets {
		_, _ = removeLeftover(filepath.Join(c.ProjectDir, d.Local))
	}
	if c.PBFURL != "" {
		_ = os.Remove(c.OSMFile())
		_ = os.Remove(statePath(c.OSMFile()))
	}
}

// chownProjectFiles gives the unprivileged user the paths it must write,
// without recursing over operator data mounted under the project directory.
func chownProjectFiles(c *Config, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}
	paths := []string{c.ProjectDir, c.EnvFilePath()}
	for _, d := range Datasets {
		paths = append(paths, filepath.Join(c.ProjectDir, d.Local))
	}
	if c.PBFURL != "" {
		paths = append(paths, c.OSMFile())
	}
	if c.FlatnodeFile != "" {
		paths = append(paths, filepath.Dir(c.FlatnodeFile), c.FlatnodeFile)
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if err := os.Lchown(p, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}
	return nil
}
