package ctl

import (
	"fmt"
	"net/url"
	"strings"
)

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
