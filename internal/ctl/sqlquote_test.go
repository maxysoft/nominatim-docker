package ctl

import "testing"

// A password reaching ALTER ROLE unescaped was arbitrary SQL execution as the
// PostgreSQL superuser.
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

// An unquoted ${POSTGRES_DB} both folded case (dropping the wrong database) and
// allowed a second statement to be appended.
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

// A connection left by a previous container used to make a re-import fail with
// "is being accessed by other users"; FORCE terminates those backends.
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
