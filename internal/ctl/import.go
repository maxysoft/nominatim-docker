package ctl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RunImport performs the one-time database provisioning and OSM import.
func RunImport(ctx context.Context, c *Config, r *Runner) error {
	if err := c.ValidateForImport(); err != nil {
		return err
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
	if err := Analyze(ctx, c.LibpqURL("nominatim", c.NominatimPassword, c.PostgresDB)); err != nil {
		return err
	}

	return cleanupDownloads(c)
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
	adminURL := c.LibpqURL(c.AdminUser, c.AdminPassword, "postgres")
	Logf("waiting for PostgreSQL at %s:%d", c.PostgresHost, c.PostgresPort)
	if err := WaitForDatabase(ctx, adminURL, 150, 2*time.Second); err != nil {
		return err
	}

	hasData, err := HasNominatimData(ctx, c.LibpqURL(c.AdminUser, c.AdminPassword, c.PostgresDB))
	if err != nil {
		return err
	}

	conn, err := Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// CREATEDB is all the import needs beyond ownership of its own database.
	// Installing PostGIS is the only step that genuinely required superuser, and
	// it is done below with the administrative connection instead.
	if err := EnsureRole(ctx, conn, "nominatim", c.NominatimPassword, c.RoleOptions); err != nil {
		return err
	}
	if err := EnsureRole(ctx, conn, c.WebUser, c.WebUserPassword, ""); err != nil {
		return err
	}

	if err := DropDatabase(ctx, conn, c.PostgresDB, hasData, c.AllowDropExistingDB); err != nil {
		return err
	}

	if c.ProvisionExtensions {
		// Installed into template1 so the database the application role creates
		// inherits them; Nominatim's own CREATE EXTENSION IF NOT EXISTS then
		// short-circuits instead of demanding superuser.
		if err := ProvisionExtensions(ctx, c.LibpqURL(c.AdminUser, c.AdminPassword, "template1")); err != nil {
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
func cleanupDownloads(c *Config) error {
	Logf("removing downloaded dumps in %s", c.ProjectDir)
	for _, d := range Datasets {
		_ = os.Remove(filepath.Join(c.ProjectDir, d.Local))
	}
	if c.PBFURL != "" {
		_ = os.Remove(c.OSMFile())
	}
	return nil
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
