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

const roleMarker = "managed by nominatim-docker"

const importMarker = "nominatim-docker: import complete"

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
		if isAuthError(err) {
			return fmt.Errorf("PostgreSQL rejected the credentials: %w", err)
		}
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

func isAuthError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "28")
}

func isMissingDatabase(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "3D000"
}

func databaseHasTables(ctx context.Context, url string) (bool, error) {
	conn, err := pgx.Connect(ctx, url)
	// Only 3D000 proves the database empty: reading any other error as empty once ended in DROP DATABASE.
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

// test/integration.sh takes the same lock by this value; change both together.
const importLockClass = 1313820489

// ponytail: the lock lives only as long as this connection, so one that dies
// mid-import (server restart, idle reaper) frees it early; re-take it before
// DROP DATABASE if that ever matters.
func lockImport(ctx context.Context, url, dbname string) (unlock func(), err error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("taking the import lock: %w", err)
	}
	// Polled, not pg_advisory_lock: a cancelled blocking wait would leave the server backend queued.
	for i := 0; ; i++ {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, hashtext($2))", importLockClass, dbname).Scan(&got); err != nil {
			conn.Close(context.Background())
			return nil, fmt.Errorf("taking the import lock: %w", err)
		}
		if got {
			return func() { conn.Close(context.Background()) }, nil
		}
		if i == 0 {
			Logf("waiting for another import into %q to finish", dbname)
		}
		select {
		case <-ctx.Done():
			conn.Close(context.Background())
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

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
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("creating role %q: %w", role, err)
		}
		defer tx.Rollback(ctx)
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

	if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s PASSWORD %s", ident, secret)); err != nil {
		return fmt.Errorf("updating password for role %q: %w", role, err)
	}
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

func reconcileRoleOptions(options string, isSuper bool) string {
	if !isSuper || roleIsSuperuser(options) ||
		slices.ContainsFunc(strings.Fields(options), func(o string) bool { return strings.EqualFold(o, "NOSUPERUSER") }) {
		return options
	}
	return strings.TrimSpace("NOSUPERUSER " + options)
}

func reconcileRoles(ctx context.Context, c *Config, url string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if err := ensureRole(ctx, conn, "nominatim", c.NominatimPassword, c.RoleOptions); err != nil {
		return err
	}
	// Calling ensureRole twice for one role would let the option-less call revoke SUPERUSER.
	if c.WebUser == "nominatim" {
		return nil
	}
	return ensureRole(ctx, conn, c.WebUser, c.WebUserPassword, "")
}

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

const userTablesSQL = `SELECT EXISTS (
	SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
	  AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'
	  AND NOT EXISTS (SELECT 1 FROM pg_depend d
	                  WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'))`

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
	force := false
	var verNum int
	// Keep the ::int cast: pgx will not scan text into an int, and this error is ignored, so FORCE would silently stop.
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&verNum); err == nil && verNum >= 130000 {
		force = true
	}
	if _, err := conn.Exec(ctx, dropDatabaseSQL(dbname, force)); err != nil {
		return fmt.Errorf("dropping database %q: %w", dbname, err)
	}
	return nil
}

func dropDatabaseSQL(dbname string, force bool) string {
	stmt := "DROP DATABASE IF EXISTS " + QuoteIdentifier(dbname)
	if force {
		stmt += " WITH (FORCE)"
	}
	return stmt
}

func provisionExtensions(ctx context.Context, conn *pgx.Conn) error {
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

func markImportComplete(ctx context.Context, conn *pgx.Conn, dbname string) error {
	_, err := conn.Exec(ctx, fmt.Sprintf("COMMENT ON DATABASE %s IS %s",
		QuoteIdentifier(dbname), QuoteLiteral(importMarker)))
	if err != nil {
		return fmt.Errorf("recording import completion: %w", err)
	}
	return nil
}

func QuoteLiteral(s string) string {
	escaped := strings.ReplaceAll(s, `'`, `''`)
	if strings.Contains(s, `\`) {
		return "E'" + strings.ReplaceAll(escaped, `\`, `\\`) + "'"
	}
	return "'" + escaped + "'"
}

func QuoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func mustNotBeEmpty(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	return nil
}

const scramIterations = 4096

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

func saslprep(password string) string {
	prepped, err := stringprep.SASLprep.Prepare(password)
	if err != nil {
		return password
	}
	return prepped
}

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
