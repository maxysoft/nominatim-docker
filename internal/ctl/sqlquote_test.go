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
