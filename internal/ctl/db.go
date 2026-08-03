package ctl

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xdg-go/stringprep"
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

// waitForDatabase polls until a connection succeeds or the attempt budget is
// exhausted, then reports the real driver error.
//
// The shell version looped forever with stderr discarded, so a wrong password
// was indistinguishable from a database that had not finished booting — and it
// hammered the server with failed authentications indefinitely.
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
		last = err
		if i == 0 || (i+1)%10 == 0 {
			Logf("waiting for PostgreSQL (attempt %d/%d): %v", i+1, attempts, Redact(err.Error()))
		}
	}
	return fmt.Errorf("PostgreSQL not reachable after %d attempts: %w", attempts, last)
}

// ensureRole creates role if it is absent, and reconciles its password.
//
// A role that exists without our marker comment belongs to someone else: on a
// shared cluster "www-data" is a common name. Resetting its password would lock
// out that application, so we stop instead.
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

// hasNominatimData reports whether the connected database holds imported tables.
// placex alone is not a completion signal — see importMarker.
func hasNominatimData(ctx context.Context, conn *pgx.Conn) bool {
	var present bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('public.placex') IS NOT NULL").Scan(&present); err != nil {
		return false
	}
	return present
}

// dropDatabase removes dbname. It refuses to touch a database that already
// contains Nominatim data unless the operator explicitly opts in.
func dropDatabase(ctx context.Context, conn *pgx.Conn, dbname string, hasData, allowed bool) error {
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

// provisionExtensions installs the extensions Nominatim requires.
//
// PostGIS is not a trusted extension, so CREATE EXTENSION needs superuser. Doing
// it here, with the administrative credentials that are already required to
// provision roles, is what lets the application role drop from SUPERUSER to
// CREATEDB.
func provisionExtensions(ctx context.Context, conn *pgx.Conn) error {
	// Must match what nominatim_db.tools.database_import.setup_database_skeleton
	// creates, in dependency order — it issues CREATE EXTENSION IF NOT EXISTS for
	// each, which succeeds without privileges only if the extension is already
	// present in the template the database was created from.
	//
	// Nominatim's setup runs `createdb`, which fails if the database already
	// exists, so the extensions cannot simply be installed into the target
	// database beforehand. template1 is the only way to hand them to an
	// unprivileged role. Only genuinely missing extensions are installed, and
	// doing so is logged, because this changes every database later created on
	// the cluster.
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

// QuoteLiteral renders s as a PostgreSQL string literal.
//
// CREATE ROLE / ALTER ROLE ... PASSWORD cannot take a bind parameter, so the
// value has to be quoted by hand. A password containing a single quote would
// otherwise terminate the literal and let the remainder execute as SQL.
func QuoteLiteral(s string) string {
	escaped := strings.ReplaceAll(s, `'`, `''`)
	if strings.Contains(s, `\`) {
		// E'' strings treat backslash as an escape character, so it must be
		// doubled; the E prefix is required for the parser to accept it at all.
		return "E'" + strings.ReplaceAll(escaped, `\`, `\\`) + "'"
	}
	return "'" + escaped + "'"
}

// QuoteIdentifier renders s as a PostgreSQL quoted identifier. Quoting also
// preserves case, so POSTGRES_DB=MyDB refers to "MyDB" rather than folding to
// mydb and acting on a different database.
func QuoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// urlEscape percent-encodes a component of a postgres:// URL.
//
// QueryEscape is wrong here: it encodes a space as "+", and the userinfo decoder
// in net/url does not map "+" back to a space. A password containing a space
// would be set correctly on the role and then fail every subsequent login.
func urlEscape(s string) string { return url.PathEscape(s) }

// mustNotBeEmpty is a small guard used where an empty identifier would produce
// syntactically valid but catastrophic SQL (e.g. DROP DATABASE "").
func mustNotBeEmpty(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	return nil
}

// scramIterations is PostgreSQL's own default for password_encryption.
const scramIterations = 4096

// scramVerifier builds the value PostgreSQL stores in pg_authid.rolpassword.
//
// `ALTER ROLE x PASSWORD 'cleartext'` sends the password to the server, which
// logs it verbatim when log_statement is 'ddl' or 'all' — on a managed provider
// that log is often shipped to a shared sink and retained for months. Computing
// the verifier here sends only a salted hash, which is what the server would
// have derived and stored anyway.
//
// Format: SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>, per
// RFC 5802 with PostgreSQL's encoding.
func scramVerifier(password string, salt []byte, iterations int) (string, error) {
	// crypto/pbkdf2 is stdlib as of Go 1.24, so x/crypto is no longer needed.
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

// saslprep normalises a password the way PostgreSQL does before hashing it.
//
// The server runs RFC 4013 SASLprep and, if that fails, falls back to the raw
// bytes (see pg_saslprep). Mirroring both halves is what lets the verifier
// computed here authenticate for any password, not just printable ASCII.
func saslprep(password string) string {
	prepped, err := stringprep.SASLprep.Prepare(password)
	if err != nil {
		return password
	}
	return prepped
}

// passwordSecret renders the value for a CREATE/ALTER ROLE ... PASSWORD clause.
// The cleartext never appears in it.
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
