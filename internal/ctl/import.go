package ctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunImport performs the one-time database provisioning and OSM import.
func RunImport(ctx context.Context, c *Config, r *Runner) error {
	if err := c.ValidateForImport(); err != nil {
		return err
	}

	// Refused before any download or provisioning: the serve image cannot
	// import, and discovering that hours in, deep inside Nominatim, helps
	// nobody.
	if !HaveImportTools() {
		return errors.New("this is the serve-only image: osm2pgsql is not installed, so it cannot run an import.\n" +
			"Run the import once with the full (non -serve) image against the same database, then start this one again")
	}

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

	tiger := filepath.Join(c.ProjectDir, "tiger-nominatim-preprocessed.csv.tar.gz")
	if _, err := os.Stat(tiger); err == nil {
		Logf("importing Tiger address data")
		if err := r.Run(ctx, "nominatim", "add-data", "--tiger-data", tiger, "--project-dir", c.ProjectDir); err != nil {
			return err
		}
	}

	// The initial import can leave parent places flagged for indexing, which
	// makes --check-database report "N entries are not yet indexed".
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

	Logf("gathering planner statistics")
	conn, err := pgx.Connect(ctx, c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB))
	if err != nil {
		return err
	}
	_, err = conn.Exec(ctx, "ANALYZE")
	conn.Close(ctx)
	if err != nil {
		return err
	}

	cleanupDownloads(c)
	return nil
}

// fetchDatasets resolves the five optional supplementary datasets.
func fetchDatasets(ctx context.Context, c *Config, dl *Downloader) error {
	for _, d := range Datasets {
		v := c.DatasetValues[d.EnvVar]
		dest := filepath.Join(c.ProjectDir, d.Local)
		switch {
		case v == "true":
			url := c.MirrorBaseURL + "/" + d.Remote
			Logf("downloading %s from %s", d.Label, url)
			if err := dl.Fetch(ctx, url, dest, os.Getenv(d.EnvVar+"_SHA256")); err != nil {
				return fmt.Errorf("%s: %w", d.Label, err)
			}
		case v != "" && v != "false":
			// Absolute-path form, already validated by Config.Validate.
			Logf("linking %s from %s", d.Label, v)
			// Remove first: a failed import previously left the symlink behind
			// and every retry then died on "file exists".
			_ = os.Remove(dest)
			if err := os.Symlink(v, dest); err != nil {
				return fmt.Errorf("%s: %w", d.Label, err)
			}
		default:
			Logf("skipping optional %s import", d.Label)
		}
	}
	return nil
}

// provisionDatabase creates the roles and clears any stale database, using the
// administrative credentials. The application role is deliberately not a
// superuser.
func provisionDatabase(ctx context.Context, c *Config) error {
	adminURL := c.LibpqURL(adminUser, c.AdminPassword, "postgres")
	Logf("waiting for PostgreSQL at %s:%d", c.PostgresHost, c.PostgresPort)
	if err := waitForDatabase(ctx, adminURL, 150, 2*time.Second); err != nil {
		return err
	}

	// Bounded: a server that accepts TCP but stalls the startup handshake would
	// otherwise block here with no diagnostic.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	defer cancelProbe()
	var hasData bool
	if tc, err := pgx.Connect(probeCtx, c.LibpqURL(adminUser, c.AdminPassword, c.PostgresDB)); err == nil {
		hasData = hasNominatimData(probeCtx, tc)
		tc.Close(probeCtx)
	}

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// CREATEDB is all the import needs beyond ownership of its own database.
	// Installing PostGIS is the only step that genuinely required superuser, and
	// it is done below with the administrative connection instead.
	if err := ensureRole(ctx, conn, "nominatim", c.NominatimPassword, c.RoleOptions); err != nil {
		return err
	}
	if err := ensureRole(ctx, conn, c.WebUser, c.WebUserPassword, ""); err != nil {
		return err
	}

	if err := dropDatabase(ctx, conn, c.PostgresDB, hasData, c.AllowDropExistingDB); err != nil {
		return err
	}

	// A superuser role installs its own extensions during the import, so there
	// is nothing to pre-seed and no reason to touch template1.
	if c.ProvisionExtensions && !strings.Contains(strings.ToUpper(c.RoleOptions), "SUPERUSER") {
		// Installed into template1 so the database the application role creates
		// inherits them; Nominatim's own CREATE EXTENSION IF NOT EXISTS then
		// short-circuits instead of demanding superuser.
		tmpl, err := pgx.Connect(ctx, c.LibpqURL(adminUser, c.AdminPassword, "template1"))
		if err != nil {
			return err
		}
		err = provisionExtensions(ctx, tmpl)
		// Closed here, not deferred: Nominatim's import runs
		// CREATE DATABASE ... TEMPLATE template1, which fails while a session
		// is still attached to template1.
		tmpl.Close(ctx)
		if err != nil {
			return fmt.Errorf("installing PostGIS into template1 (set PROVISION_EXTENSIONS=false if your "+
				"provider manages extensions, or NOMINATIM_ROLE_OPTIONS=SUPERUSER to let the import do it): %w", err)
		}
	}
	return nil
}

// configureReplicationOrFreeze mirrors the original post-import branch.
func configureReplicationOrFreeze(ctx context.Context, c *Config, r *Runner, dl *Downloader) error {
	if c.ReplicationURL != "" && !dl.Reachable(ctx, c.ReplicationURL, 3, 2*time.Second) {
		Logf("WARNING: REPLICATION_URL unreachable; continuing without replication")
		c.ReplicationURL = ""
		if err := WriteEnvFile(c, r.UID, r.GID); err != nil {
			return err
		}
	}
	if c.ReplicationURL != "" {
		if c.Freeze {
			Logf("skipping freeze because REPLICATION_URL is set")
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

// cleanupDownloads removes exactly the files the import created. The previous
// `rm -f ${PROJECT_DIR}/*sql.gz` also matched operator files that happened to
// end in those characters.
func cleanupDownloads(c *Config) {
	Logf("removing downloaded dumps in %s", c.ProjectDir)
	for _, d := range Datasets {
		_ = os.Remove(filepath.Join(c.ProjectDir, d.Local))
	}
	if c.PBFURL != "" {
		_ = os.Remove(c.OSMFile())
	}
}

// chownProjectFiles gives the unprivileged user ownership of the paths it has
// to write, without recursing over operator data mounted under the project
// directory.
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
