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
	if err := markImportComplete(ctx, conn, c.PostgresDB); err != nil {
		return err
	}

	cleanupDownloads(c)
	return nil
}

func fetchDatasets(ctx context.Context, c *Config, dl *Downloader) error {
	for _, d := range Datasets {
		v := c.DatasetValues[d.EnvVar]
		dest := filepath.Join(c.ProjectDir, d.Local)
		switch {
		case v == "true":
			if isOperatorFile(dest) {
				return operatorFileError(d, dest)
			}
			url := c.MirrorBaseURL + "/" + d.Remote
			Logf("downloading %s from %s", d.Label, url)
			if err := dl.Fetch(ctx, url, dest, os.Getenv(d.EnvVar+"_SHA256")); err != nil {
				return fmt.Errorf("%s: %w", d.Label, err)
			}
		case v != "" && v != "false":
			if isSameRegularFile(v, dest) {
				Logf("using %s from %s", d.Label, v)
				// Drop our state file, or cleanupDownloads would delete the operator's file.
				_ = os.Remove(statePath(dest))
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
			if kept && d.EnvVar != "IMPORT_TIGER_ADDRESSES" {
				Logf("WARNING: %s is not set, but %s exists; Nominatim imports it anyway", d.EnvVar, dest)
				continue
			}
			Logf("skipping optional %s import", d.Label)
		}
	}
	return nil
}

func isSameRegularFile(path, dest string) bool {
	pi, err := os.Stat(path)
	if err != nil {
		return false
	}
	di, err := os.Lstat(dest)
	return err == nil && di.Mode().IsRegular() && os.SameFile(pi, di)
}

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

func provisionDatabase(ctx context.Context, c *Config) error {
	adminURL := c.LibpqURL(adminUser, c.AdminPassword, "postgres")
	Logf("waiting for PostgreSQL at %s:%d", c.PostgresHost, c.PostgresPort)
	if err := waitForDatabase(ctx, adminURL, 150, 2*time.Second); err != nil {
		return err
	}

	probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	defer cancelProbe()
	hasData, err := databaseHasTables(probeCtx, c.LibpqURL(adminUser, c.AdminPassword, c.PostgresDB))
	if err != nil {
		if !c.AllowDropExistingDB {
			return fmt.Errorf("cannot inspect database %q, refusing to drop it: %w", c.PostgresDB, err)
		}
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

	if c.ProvisionExtensions && !roleIsSuperuser(c.RoleOptions) {
		tmpl, err := pgx.Connect(ctx, c.LibpqURL(adminUser, c.AdminPassword, "template1"))
		if err != nil {
			return err
		}
		err = provisionExtensions(ctx, tmpl)
		// Not deferred: CREATE DATABASE ... TEMPLATE template1 fails while this session is still attached.
		tmpl.Close(ctx)
		if err != nil {
			return fmt.Errorf("installing PostGIS into template1 (set PROVISION_EXTENSIONS=false if your "+
				"provider manages extensions, or NOMINATIM_ROLE_OPTIONS=SUPERUSER to let the import do it): %w", err)
		}
	}
	return nil
}

func roleIsSuperuser(options string) bool {
	return slices.ContainsFunc(strings.Fields(options), func(o string) bool { return strings.EqualFold(o, "SUPERUSER") })
}

func configureReplicationOrFreeze(ctx context.Context, c *Config, r *Runner, dl *Downloader) error {
	if c.ReplicationURL != "" {
		if c.Freeze {
			Logf("skipping freeze because REPLICATION_URL is set")
		}
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

func warmCaches(ctx context.Context, c *Config, r *Runner) error {
	args := []string{"admin", "--warm", "--project-dir", c.ProjectDir}
	if c.ReverseOnly {
		args = append(args, "--reverse")
	}
	warm := r.WithEnv("NOMINATIM_QUERY_TIMEOUT=600", "NOMINATIM_REQUEST_TIMEOUT=3600")
	return warm.Run(ctx, "nominatim", args...)
}

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
