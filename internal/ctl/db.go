package ctl

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// roleMarker tags the roles this image manages, so a shared PostgreSQL server
// hosting a pre-existing "www-data" or "nominatim" role is never silently
// hijacked by resetting its password.
const roleMarker = "managed by nominatim-docker"

// importMarker is written as a COMMENT ON DATABASE only after an import has
// fully succeeded. `public.placex` alone is not a completion signal: it is
// created early, so an import killed part-way leaves it behind and the database
// would then be served as though it were finished.
const importMarker = "nominatim-docker: import complete"

// Connect opens a single connection.
func Connect(ctx context.Context, url string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, url)
}

// WaitForDatabase polls until a connection succeeds or the attempt budget is
// exhausted, then reports the real driver error.
//
// The shell version looped forever with stderr discarded, so a wrong password
// was indistinguishable from a database that had not finished booting — and it
// hammered the server with failed authentications indefinitely.
func WaitForDatabase(ctx context.Context, url string, attempts int, delay time.Duration) error {
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := pgx.Connect(cctx, url)
		cancel()
		if err == nil {
			conn.Close(ctx)
			return nil
		}
		last = err
		if i == 0 || (i+1)%10 == 0 {
			Logf("waiting for PostgreSQL (attempt %d/%d): %v", i+1, attempts, Redact(err.Error()))
		}
	}
	return fmt.Errorf("PostgreSQL not reachable after %d attempts: %w", attempts, last)
}

// EnsureRole creates role if it is absent, and reconciles its password.
//
// A role that exists without our marker comment belongs to someone else: on a
// shared cluster "www-data" is a common name. Resetting its password would lock
// out that application, so we stop instead.
func EnsureRole(ctx context.Context, conn *pgx.Conn, role, password string, extraOptions string) error {
	if err := mustNotBeEmpty("role name", role); err != nil {
		return err
	}

	var exists bool
	if qErr := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); qErr != nil {
		return fmt.Errorf("checking role %q: %w", role, qErr)
	}

	ident := QuoteIdentifier(role)
	secret, hashed, err := passwordSecret(password)
	if err != nil {
		return err
	}
	if !hashed {
		Logf("WARNING: password for role %q is not printable ASCII; sending it to the server "+
			"in cleartext, where log_statement=ddl|all would record it", role)
	}

	if !exists {
		Logf("creating role %s", role)
		stmt := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s", ident, secret)
		if extraOptions != "" {
			stmt += " " + extraOptions
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("creating role %q: %w", role, err)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("COMMENT ON ROLE %s IS %s", ident, QuoteLiteral(roleMarker))); err != nil {
			return fmt.Errorf("marking role %q: %w", role, err)
		}
		return nil
	}

	var comment *string
	if err := conn.QueryRow(ctx, "SELECT shobj_description(oid, 'pg_authid') FROM pg_roles WHERE rolname = $1", role).Scan(&comment); err != nil {
		return fmt.Errorf("reading comment on role %q: %w", role, err)
	}
	if comment == nil || *comment != roleMarker {
		return fmt.Errorf("role %q already exists and is not managed by this image; "+
			"refusing to change its password. Drop it, or comment it with %q to adopt it", role, roleMarker)
	}

	// Managed role: keep the password in step with the environment so that
	// rotating NOMINATIM_PASSWORD actually takes effect.
	if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s PASSWORD %s", ident, secret)); err != nil {
		return fmt.Errorf("updating password for role %q: %w", role, err)
	}
	// Reconcile attributes as well, otherwise NOMINATIM_ROLE_OPTIONS would only
	// ever apply on the run that created the role — useless as an escape hatch.
	if extraOptions != "" {
		if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s %s", ident, extraOptions)); err != nil {
			return fmt.Errorf("applying options %q to role %q: %w", extraOptions, role, err)
		}
	}
	return nil
}

// HasNominatimData reports whether the target database already holds an
// imported Nominatim schema.
//
// This is the authoritative import marker. The previous implementation keyed
// off a file in the application volume while the data lived in the database
// volume; the two have independent lifecycles, so removing or renaming the
// application volume made the container drop a fully populated database.
func HasNominatimData(ctx context.Context, url string) (bool, error) {
	// A database that is absent, or that we cannot yet authenticate against,
	// simply holds no data as far as this check is concerned. Connectivity is
	// verified separately by WaitForDatabase, which reports the real error.
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, err := pgx.Connect(cctx, url)
	if err != nil {
		return false, nil
	}
	defer conn.Close(ctx)

	var present bool
	if err := conn.QueryRow(cctx, "SELECT to_regclass('public.placex') IS NOT NULL").Scan(&present); err != nil {
		return false, nil
	}
	return present, nil
}

// DropDatabase removes dbname. It refuses to touch a database that already
// contains Nominatim data unless the operator explicitly opts in.
func DropDatabase(ctx context.Context, conn *pgx.Conn, dbname string, hasData, allowed bool) error {
	if err := mustNotBeEmpty("POSTGRES_DB", dbname); err != nil {
		return err
	}
	if hasData && !allowed {
		return fmt.Errorf("database %q already contains an imported Nominatim schema (public.placex exists). "+
			"Refusing to drop it. Set ALLOW_DROP_EXISTING_DB=true to overwrite, or point POSTGRES_DB at a different database", dbname)
	}
	if hasData {
		Logf("WARNING: ALLOW_DROP_EXISTING_DB=true — dropping populated database %q", dbname)
	}
	// A connection left behind by a previous container makes a plain DROP fail
	// with "is being accessed by other users". FORCE terminates those backends;
	// it needs PostgreSQL 13+, so fall back when the server is older.
	force := false
	var verNum int
	if err := conn.QueryRow(ctx, "SHOW server_version_num").Scan(&verNum); err == nil && verNum >= 130000 {
		force = true
	}
	// The identifier is quoted: an unquoted ${POSTGRES_DB} both folds case and
	// allows a second statement to be appended.
	if _, err := conn.Exec(ctx, dropDatabaseSQL(dbname, force)); err != nil {
		return fmt.Errorf("dropping database %q: %w", dbname, err)
	}
	return nil
}

// dropDatabaseSQL builds the DROP statement. Split out so the quoting and the
// FORCE selection are testable without a server.
func dropDatabaseSQL(dbname string, force bool) string {
	stmt := "DROP DATABASE IF EXISTS " + QuoteIdentifier(dbname)
	if force {
		stmt += " WITH (FORCE)"
	}
	return stmt
}

// ProvisionExtensions installs the extensions Nominatim requires.
//
// PostGIS is not a trusted extension, so CREATE EXTENSION needs superuser. Doing
// it here, with the administrative credentials that are already required to
// provision roles, is what lets the application role drop from SUPERUSER to
// CREATEDB.
func ProvisionExtensions(ctx context.Context, url string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// Must match what nominatim_db.tools.database_import.setup_database_skeleton
	// creates, in dependency order — it issues CREATE EXTENSION IF NOT EXISTS for
	// each, which succeeds without privileges only if the extension is already
	// present in the template the database was created from.
	for _, ext := range []string{"hstore", "postgis", "postgis_raster"} {
		if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+QuoteIdentifier(ext)); err != nil {
			return fmt.Errorf("creating extension %s: %w", ext, err)
		}
	}
	return nil
}

// ImportComplete reports whether a finished import is recorded on the database.
func ImportComplete(ctx context.Context, url, dbname string) (bool, error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return false, nil // absent or unreachable database holds no import
	}
	defer conn.Close(ctx)

	var comment *string
	err = conn.QueryRow(ctx,
		"SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1",
		dbname).Scan(&comment)
	if err != nil {
		return false, nil
	}
	return comment != nil && *comment == importMarker, nil
}

// MarkImportComplete records that the import finished. Requires ownership of
// the database, which the application role has.
func MarkImportComplete(ctx context.Context, url, dbname string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, fmt.Sprintf("COMMENT ON DATABASE %s IS %s",
		QuoteIdentifier(dbname), QuoteLiteral(importMarker)))
	if err != nil {
		return fmt.Errorf("recording import completion: %w", err)
	}
	return nil
}

// Analyze refreshes planner statistics after the import.
func Analyze(ctx context.Context, url string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "ANALYZE")
	return err
}
