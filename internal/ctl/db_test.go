package ctl

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsAuthError(t *testing.T) {
	if !isAuthError(fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01"})) {
		t.Fatal("wrapped invalid_password not recognised")
	}
	if !isAuthError(&pgconn.PgError{Code: "28000"}) {
		t.Fatal("invalid_authorization_specification not recognised")
	}
	if isAuthError(&pgconn.PgError{Code: "57P03"}) {
		t.Fatal("a server still starting up must be retried")
	}
	if isAuthError(errors.New("dial tcp: connection refused")) {
		t.Fatal("a refused connection must be retried")
	}
}

func TestIsMissingDatabase(t *testing.T) {
	if !isMissingDatabase(fmt.Errorf("connect: %w", &pgconn.PgError{Code: "3D000"})) {
		t.Fatal("wrapped invalid_catalog_name not recognised")
	}
	for _, err := range []error{
		nil,
		&pgconn.PgError{Code: "53300"},
		&pgconn.PgError{Code: "28000"},
		errors.New("dial tcp: i/o timeout"),
	} {
		if isMissingDatabase(err) {
			t.Fatalf("%v must not count as a missing database", err)
		}
	}
}

func TestRoleIsSuperuser(t *testing.T) {
	for opts, want := range map[string]bool{
		"":                     false,
		"CREATEDB":             false,
		"NOSUPERUSER CREATEDB": false,
		"SUPERUSER":            true,
		"createdb superuser":   true,
	} {
		if got := roleIsSuperuser(opts); got != want {
			t.Errorf("roleIsSuperuser(%q) = %v, want %v", opts, got, want)
		}
	}
}

func TestReconcileRoleOptions(t *testing.T) {
	for _, tc := range []struct {
		opts  string
		super bool
		want  string
	}{
		{"CREATEDB", true, "NOSUPERUSER CREATEDB"},
		{"", true, "NOSUPERUSER"},
		{"SUPERUSER", true, "SUPERUSER"},
		{"NOSUPERUSER CREATEDB", true, "NOSUPERUSER CREATEDB"},
		{"CREATEDB", false, "CREATEDB"},
		{"", false, ""},
	} {
		if got := reconcileRoleOptions(tc.opts, tc.super); got != tc.want {
			t.Errorf("reconcileRoleOptions(%q, %v) = %q, want %q", tc.opts, tc.super, got, tc.want)
		}
	}
}

func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"simple": "'simple'",
		"pa'ss":  "'pa''ss'",
		`x'; ALTER ROLE postgres PASSWORD 'owned'; --`: `'x''; ALTER ROLE postgres PASSWORD ''owned''; --'`,
		`back\slash`: `E'back\\slash'`,
		"":           "''",
	}
	for in, want := range cases {
		if got := QuoteLiteral(in); got != want {
			t.Errorf("QuoteLiteral(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestQuoteIdentifier(t *testing.T) {
	cases := map[string]string{
		"nominatim":                      `"nominatim"`,
		"My-DB":                          `"My-DB"`,
		`nominatim"; DROP DATABASE prod`: `"nominatim""; DROP DATABASE prod"`,
	}
	for in, want := range cases {
		if got := QuoteIdentifier(in); got != want {
			t.Errorf("QuoteIdentifier(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestMustNotBeEmpty(t *testing.T) {
	if err := mustNotBeEmpty("POSTGRES_DB", ""); err == nil {
		t.Fatal("empty value accepted")
	}
	if err := mustNotBeEmpty("POSTGRES_DB", "   "); err == nil {
		t.Fatal("whitespace-only value accepted")
	}
	if err := mustNotBeEmpty("POSTGRES_DB", "nominatim"); err != nil {
		t.Fatalf("valid value rejected: %v", err)
	}
}

func TestDropDatabaseSQL(t *testing.T) {
	cases := map[bool]string{
		true:  `DROP DATABASE IF EXISTS "nominatim" WITH (FORCE)`,
		false: `DROP DATABASE IF EXISTS "nominatim"`,
	}
	for force, want := range cases {
		if got := dropDatabaseSQL("nominatim", force); got != want {
			t.Errorf("dropDatabaseSQL(force=%v) = %q, want %q", force, got, want)
		}
	}
	if got := dropDatabaseSQL(`x"; DROP DATABASE prod`, true); got != `DROP DATABASE IF EXISTS "x""; DROP DATABASE prod" WITH (FORCE)` {
		t.Errorf("identifier not quoted under FORCE: %s", got)
	}
}

func TestScramVerifierMatchesReferenceImplementation(t *testing.T) {
	const want = "SCRAM-SHA-256$4096:MDEyMzQ1Njc4OWFiY2RlZg==$" +
		"ZIQVNqStZRzlOhIpyOxF6+ntWHcrs3R/7ZWYkCqWWoc=:2ieFiDOjSo2BYGVmJhMdwofseSoibXz2jJQZfuPwDxA="

	got, err := scramVerifier("s3cret-password", []byte("0123456789abcdef"), 4096)
	if err != nil {
		t.Fatalf("ScramVerifier: %v", err)
	}
	if got != want {
		t.Fatalf("verifier mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestPasswordSecretHidesTheCleartext(t *testing.T) {
	for _, pw := range []string{"s3cret-password", "pässwort", "naïve", "emoji🙂", "with space"} {
		sql, err := passwordSecret(pw)
		if err != nil {
			t.Fatalf("passwordSecret(%q): %v", pw, err)
		}
		if strings.Contains(sql, pw) {
			t.Errorf("cleartext password %q present in the statement: %s", pw, sql)
		}
		if !strings.HasPrefix(sql, "'SCRAM-SHA-256$4096:") {
			t.Errorf("unexpected verifier shape for %q: %s", pw, sql)
		}
	}
}

func TestPasswordSecretSaltsEachCall(t *testing.T) {
	a, err := passwordSecret("pw")
	if err != nil {
		t.Fatal(err)
	}
	b, err := passwordSecret("pw")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("expected a fresh random salt on each call")
	}
}

func TestSASLprepMatchesRFC4013Examples(t *testing.T) {
	cases := map[string]string{
		"I\u00ADX": "IX",
		"user":     "user",
		"USER":     "USER",
		"\u00AA":   "a",
		"\u2168":   "IX",
		"a\u00A0b": "a b",
		"simple":   "simple",
	}
	for in, want := range cases {
		if got := saslprep(in); got != want {
			t.Errorf("saslprep(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSASLprepFallsBackOnProhibitedInput(t *testing.T) {
	in := "bad\u0007control"
	if got := saslprep(in); got != in {
		t.Fatalf("saslprep(%q) = %q, want the input unchanged", in, got)
	}
}

func TestSASLprepIsIdempotent(t *testing.T) {
	for _, pw := range []string{
		"simple", "pässwort", "I­X", "a b", "Ωhm", "emoji🙂", "with space",
	} {
		once := saslprep(pw)
		if twice := saslprep(once); twice != once {
			t.Errorf("saslprep is not idempotent for %q: %q -> %q", pw, once, twice)
		}
	}
}

func TestLibpqURLCarriesPreparedPassword(t *testing.T) {
	withEnv(t, baseEnv())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a, b := c.LibpqURL("u", "I­X", "db"), c.LibpqURL("u", "IX", "db"); a != b {
		t.Fatalf("URL not built from the prepared password:\n%s\n%s", a, b)
	}
}
