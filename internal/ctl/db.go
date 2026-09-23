package ctl

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/xdg-go/stringprep"
)

// roleMarker tags the roles this image manages, so a pre-existing "www-data"
// or "nominatim" on a shared server is never silently hijacked.
const roleMarker = "managed by nominatim-docker"

// importMarker is written as a COMMENT ON DATABASE only after an import has
// fully succeeded; public.placex alone is created too early to be a signal.
const importMarker = "nominatim-docker: import complete"

// waitForDatabase polls until a connection succeeds or the attempt budget is
// exhausted, then reports the real driver error.
func waitForDatabase(ctx context.Context, url string, attempts int, delay time.Duration) error {
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
		// A rejected password is final; retrying only delays the same error.
		if isAuthError(err) {
			return fmt.Errorf("PostgreSQL rejected the credentials: %w", err)
		}
		// So is a missing database: the server answered, and the caller
		// decides what absence means.
		if isMissingDatabase(err) {
			return fmt.Errorf("PostgreSQL is reachable but the database is missing: %w", err)
		}
		last = err
		if i == 0 || (i+1)%10 == 0 {
			Logf("waiting for PostgreSQL (attempt %d/%d): %v", i+1, attempts, err)
		}
	}
	return fmt.Errorf("PostgreSQL not reachable after %d attempts: %w", attempts, last)
}

// isAuthError reports SQLSTATE class 28 (invalid authorization). Startup (57P03)
// and refused connections keep retrying.
func isAuthError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "28")
}

// isMissingDatabase reports SQLSTATE 3D000 (invalid_catalog_name): the
// database does not exist. It is the only connect error that proves a
// database holds nothing; every other one leaves its contents unknown.
func isMissingDatabase(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "3D000"
}

// databaseHasTables connects to url and reports whether its database holds
// any tables. A database that does not exist holds none. Any other failure is
// returned, never read as "empty": that reading once let a transient error end
// in DROP DATABASE on a finished import.
func databaseHasTables(ctx context.Context, url string) (bool, error) {
	conn, err := pgx.Connect(ctx, url)
	if isMissingDatabase(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer conn.Close(ctx)
	var present bool
	err = conn.QueryRow(ctx, userTablesSQL).Scan(&present)
	return present, err
}

// readImportMarker connects to url and reports whether dbname carries the
// completion marker. The marker lives in pg_database, so a connection to the
// maintenance database works even where pg_hba keeps the admin out of dbname.
// A database left invalid by an interrupted DROP (datconnlimit -2, PostgreSQL
// 16+) can keep its comment, so it never counts as complete.
func readImportMarker(ctx context.Context, url, dbname string) (bool, error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return false, err
	}
	defer conn.Close(ctx)
	var comment *string
	err = conn.QueryRow(ctx,
		"SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1 AND datconnlimit <> -2",
		dbname).Scan(&comment)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return comment != nil && *comment == importMarker, nil
}

// ensureRole creates role if it is absent, and reconciles its password and
// attributes. A role without our marker comment belongs to someone else, so
// we stop rather than reset its password.
func ensureRole(ctx context.Context, conn *pgx.Conn, role, password string, extraOptions string) error {
	if err := mustNotBeEmpty("role name", role); err != nil {
		return err
	}

	var exists bool
	if qErr := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); qErr != nil {
		return fmt.Errorf("checking role %q: %w", role, qErr)
	}

	ident := QuoteIdentifier(role)
	secret, err := passwordSecret(password)
	if err != nil {
		return err
	}

	if !exists {
		Logf("creating role %s", role)
		stmt := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s", ident, secret)
		if extraOptions != "" {
			stmt += " " + extraOptions
		}
		// One transaction: a role created without its marker would be refused
		// as foreign on every later start.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("creating role %q: %w", role, err)
		}
		defer tx.Rollback(ctx) // no-op after Commit
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("creating role %q: %w", role, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("COMMENT ON ROLE %s IS %s", ident, QuoteLiteral(roleMarker))); err != nil {
			return fmt.Errorf("marking role %q: %w", role, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("creating role %q: %w", role, err)
		}
		return nil
	}

	var comment *string
	var super bool
	if err := conn.QueryRow(ctx, "SELECT shobj_description(oid, 'pg_authid'), rolsuper FROM pg_roles WHERE rolname = $1", role).Scan(&comment, &super); err != nil {
		return fmt.Errorf("reading comment on role %q: %w", role, err)
	}
	if comment == nil || *comment != roleMarker {
		return fmt.Errorf("role %q already exists and is not managed by this image; "+
			"refusing to change its password. Drop it, or comment it with %q to adopt it", role, roleMarker)
	}

	// Keep the password in step with the environment so rotation takes effect.
	if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s PASSWORD %s", ident, secret)); err != nil {
		return fmt.Errorf("updating password for role %q: %w", role, err)
	}
	// Reconcile attributes too, or NOMINATIM_ROLE_OPTIONS would only apply on
	// the run that created the role.
	opts := reconcileRoleOptions(extraOptions, super)
	if opts != extraOptions {
		Logf("revoking SUPERUSER from role %s: its options no longer grant it", role)
	}
	if opts != "" {
		if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s %s", ident, opts)); err != nil {
			return fmt.Errorf("applying options %q to role %q: %w", opts, role, err)
		}
	}
	return nil
}

// reconcileRoleOptions adds NOSUPERUSER when a role is a superuser that the
// options no longer make one, so dropping SUPERUSER from NOMINATIM_ROLE_OPTIONS
// takes effect. Only then: NOSUPERUSER itself needs superuser to issue.
func reconcileRoleOptions(options string, isSuper bool) string {
	if !isSuper || roleIsSuperuser(options) ||
		slices.ContainsFunc(strings.Fields(options), func(o string) bool { return strings.EqualFold(o, "NOSUPERUSER") }) {
		return options
	}
	return strings.TrimSpace("NOSUPERUSER " + options)
}

// reconcileRoles creates or updates the application and web roles at url, so a
// rotated password takes effect on the next start.
func reconcileRoles(ctx context.Context, c *Config, url string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// CREATEDB is all the import needs once PostGIS is pre-installed.
	if err := ensureRole(ctx, conn, "nominatim", c.NominatimPassword, c.RoleOptions); err != nil {
		return err
	}
	// The same role twice would have its options reconciled away by the
	// second, option-less call (SUPERUSER revoked).
	if c.WebUser == "nominatim" {
		return nil
	}
	return ensureRole(ctx, conn, c.WebUser, c.WebUserPassword, "")
}

// databaseState reports whether dbname is frozen, by Nominatim's own test (the
// place table is gone after `nominatim freeze`), and whether the application
// role owns it (an adopted database may belong to someone else).
func databaseState(ctx context.Context, url, dbname string) (frozen, ownedByApp bool, err error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return false, false, err
	}
	defer conn.Close(ctx)
	err = conn.QueryRow(ctx,
		"SELECT to_regclass('public.place') IS NULL, pg_get_userbyid(datdba) = 'nominatim' FROM pg_database WHERE datname = $1",
		dbname).Scan(&frozen, &ownedByApp)
	if err != nil {
		return false, false, fmt.Errorf("inspecting database %q: %w", dbname, err)
	}
	return frozen, ownedByApp, nil
}

// userTablesSQL finds any relation that is not a system catalog and not owned
// by an extension (PostGIS's spatial_ref_sys comes from template1). Checking
// only public.placex would let a non-Nominatim database on a shared server be
// dropped without ALLOW_DROP_EXISTING_DB.
const userTablesSQL = `SELECT EXISTS (
	SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
	  AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'
	  AND NOT EXISTS (SELECT 1 FROM pg_depend d
	                  WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'))`

// dropDatabase removes dbname, refusing to touch a populated database unless
// the operator explicitly opted in.
func dropDatabase(ctx context.Context, conn *pgx.Conn, dbname string, hasData, allowed bool) error {
	if err := mustNotBeEmpty("POSTGRES_DB", dbname); err != nil {
		return err
	}
	if hasData && !allowed {
		return fmt.Errorf("database %q already contains tables. "+
			"Refusing to drop it. Set ALLOW_DROP_EXISTING_DB=true to overwrite, or point POSTGRES_DB at a different database", dbname)
	}
	if hasData {
		Logf("WARNING: ALLOW_DROP_EXISTING_DB=true, dropping populated database %q", dbname)
	}
	// FORCE terminates connections left by a previous container; PostgreSQL 13+.
	// Cast on the server: SHOW yields text, which pgx will not scan into an int.
	force := false
	var verNum int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&verNum); err == nil && verNum >= 130000 {
		force = true
	}
	if _, err := conn.Exec(ctx, dropDatabaseSQL(dbname, force)); err != nil {
		return fmt.Errorf("dropping database %q: %w", dbname, err)
	}
	return nil
}

// dropDatabaseSQL builds the DROP statement; split out so the quoting and the
// FORCE selection are testable without a server.
func dropDatabaseSQL(dbname string, force bool) string {
	stmt := "DROP DATABASE IF EXISTS " + QuoteIdentifier(dbname)
	if force {
		stmt += " WITH (FORCE)"
	}
	return stmt
}

// provisionExtensions installs the extensions Nominatim requires into the
// connected database (template1): PostGIS is untrusted, so CREATE EXTENSION
// needs superuser, and template1 is the only way to hand the extensions to an
// unprivileged role, because Nominatim's own createdb fails on an existing database.
func provisionExtensions(ctx context.Context, conn *pgx.Conn) error {
	// Must match what nominatim_db's setup_database_skeleton creates.
	var missing []string
	for _, ext := range []string{"hstore", "postgis", "postgis_raster"} {
		var present bool
		if err := conn.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", ext).Scan(&present); err != nil {
			return fmt.Errorf("checking extension %s: %w", ext, err)
		}
		if !present {
			missing = append(missing, ext)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	Logf("NOTE: installing %v into template1. Every database created on this "+
		"server from now on will inherit them. Set PROVISION_EXTENSIONS=false to "+
		"manage extensions yourself.", missing)
	for _, ext := range missing {
		if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+QuoteIdentifier(ext)); err != nil {
			return fmt.Errorf("creating extension %s: %w", ext, err)
		}
	}
	return nil
}

// markImportComplete records that the import finished. Requires ownership of
// the database, which the application role has.
func markImportComplete(ctx context.Context, conn *pgx.Conn, dbname string) error {
	_, err := conn.Exec(ctx, fmt.Sprintf("COMMENT ON DATABASE %s IS %s",
		QuoteIdentifier(dbname), QuoteLiteral(importMarker)))
	if err != nil {
		return fmt.Errorf("recording import completion: %w", err)
	}
	return nil
}

// QuoteLiteral renders s as a PostgreSQL string literal. CREATE/ALTER ROLE
// ... PASSWORD cannot take a bind parameter, and an unescaped quote would let
// the remainder execute as SQL.
func QuoteLiteral(s string) string {
	escaped := strings.ReplaceAll(s, `'`, `''`)
	if strings.Contains(s, `\`) {
		// E'' strings treat backslash as an escape, so double it.
		return "E'" + strings.ReplaceAll(escaped, `\`, `\\`) + "'"
	}
	return "'" + escaped + "'"
}

// QuoteIdentifier renders s as a PostgreSQL quoted identifier; quoting also
// stops POSTGRES_DB=MyDB from case-folding to a different database.
func QuoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// mustNotBeEmpty guards identifiers where an empty value would produce valid
// but catastrophic SQL (e.g. DROP DATABASE "").
func mustNotBeEmpty(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	return nil
}

// scramIterations is PostgreSQL's own default for password_encryption.
const scramIterations = 4096

// scramVerifier builds the value PostgreSQL stores in pg_authid.rolpassword,
// so the cleartext password never reaches the server, where log_statement
// would record it verbatim. Format per RFC 5802 with PostgreSQL's encoding.
func scramVerifier(password string, salt []byte, iterations int) (string, error) {
	saltedPassword, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("deriving SCRAM key: %w", err)
	}

	mac := func(msg string) []byte {
		m := hmac.New(sha256.New, saltedPassword)
		m.Write([]byte(msg))
		return m.Sum(nil)
	}
	storedKey := sha256.Sum256(mac("Client Key"))
	serverKey := mac("Server Key")

	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

// saslprep normalises a password the way PostgreSQL does before hashing:
// RFC 4013, falling back to the raw bytes when preparation fails
// (mirroring pg_saslprep), so non-ASCII passwords verify too.
func saslprep(password string) string {
	prepped, err := stringprep.SASLprep.Prepare(password)
	if err != nil {
		return password
	}
	return prepped
}

// passwordSecret renders the value for a CREATE/ALTER ROLE ... PASSWORD
// clause; the cleartext never appears in it.
func passwordSecret(password string) (sql string, err error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating SCRAM salt: %w", err)
	}
	verifier, err := scramVerifier(saslprep(password), salt, scramIterations)
	if err != nil {
		return "", err
	}
	return QuoteLiteral(verifier), nil
}
