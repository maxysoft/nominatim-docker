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
	"net/url"
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
		last = err
		if i == 0 || (i+1)%10 == 0 {
			Logf("waiting for PostgreSQL (attempt %d/%d): %v", i+1, attempts, Redact(err.Error()))
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

	// Keep the password in step with the environment so rotation takes effect.
	if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s PASSWORD %s", ident, secret)); err != nil {
		return fmt.Errorf("updating password for role %q: %w", role, err)
	}
	// Reconcile attributes too, or NOMINATIM_ROLE_OPTIONS would only apply on
	// the run that created the role.
	if extraOptions != "" {
		if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s %s", ident, extraOptions)); err != nil {
			return fmt.Errorf("applying options %q to role %q: %w", extraOptions, role, err)
		}
	}
	return nil
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
	return ensureRole(ctx, conn, c.WebUser, c.WebUserPassword, "")
}

// hasNominatimData reports whether the connected database holds imported
// tables; see importMarker for why this alone is not a completion signal.
func hasNominatimData(ctx context.Context, conn *pgx.Conn) bool {
	var present bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('public.placex') IS NOT NULL").Scan(&present); err != nil {
		return false
	}
	return present
}

// dropDatabase removes dbname, refusing to touch a populated database unless
// the operator explicitly opted in.
func dropDatabase(ctx context.Context, conn *pgx.Conn, dbname string, hasData, allowed bool) error {
	if err := mustNotBeEmpty("POSTGRES_DB", dbname); err != nil {
		return err
	}
	if hasData && !allowed {
		return fmt.Errorf("database %q already contains an imported Nominatim schema (public.placex exists). "+
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

// importComplete reports whether a finished import is recorded. Reads
// pg_database, so any connection to the server will do.
func importComplete(ctx context.Context, conn *pgx.Conn, dbname string) bool {
	var comment *string
	if err := conn.QueryRow(ctx,
		"SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1",
		dbname).Scan(&comment); err != nil {
		return false
	}
	return comment != nil && *comment == importMarker
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

// urlEscape percent-encodes a component of a postgres:// URL. QueryEscape is
// wrong here: it encodes a space as "+", which the userinfo decoder does not
// map back, so such a password would fail every login.
func urlEscape(s string) string { return url.PathEscape(s) }

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
