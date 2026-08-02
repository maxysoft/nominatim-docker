package ctl

import (
	"strings"
	"testing"
)

// Cross-checked against an independent implementation (Python hashlib/hmac)
// rather than against this package's own output, so a mistake in the algorithm
// cannot make the test pass alongside it.
func TestScramVerifierMatchesReferenceImplementation(t *testing.T) {
	const want = "SCRAM-SHA-256$4096:MDEyMzQ1Njc4OWFiY2RlZg==$" +
		"ZIQVNqStZRzlOhIpyOxF6+ntWHcrs3R/7ZWYkCqWWoc=:2ieFiDOjSo2BYGVmJhMdwofseSoibXz2jJQZfuPwDxA="

	got := ScramVerifier("s3cret-password", []byte("0123456789abcdef"), 4096)
	if got != want {
		t.Fatalf("verifier mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestScramVerifierIsDeterministicForAGivenSalt(t *testing.T) {
	salt := []byte("fixed-salt-16byt")
	a := ScramVerifier("pw", salt, scramIterations)
	b := ScramVerifier("pw", salt, scramIterations)
	if a != b {
		t.Fatal("verifier is not deterministic for a fixed salt")
	}
	if c := ScramVerifier("pw", []byte("different-salt16"), scramIterations); c == a {
		t.Fatal("a different salt produced the same verifier")
	}
	if c := ScramVerifier("other", salt, scramIterations); c == a {
		t.Fatal("a different password produced the same verifier")
	}
}

// The whole point: the cleartext password must not appear in the statement,
// for any password — not only printable ASCII.
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

// A fresh salt per call means two invocations differ even for one password.
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

// RFC 4013 worked examples. PostgreSQL applies the same transformation before
// hashing, so a mismatch here means the verifier will not authenticate.
func TestSASLprepMatchesRFC4013Examples(t *testing.T) {
	cases := map[string]string{
		"I\u00ADX": "IX",     // SOFT HYPHEN is mapped to nothing
		"user":     "user",   // unchanged
		"USER":     "USER",   // no case folding
		"\u00AA":   "a",      // FEMININE ORDINAL INDICATOR normalises under NFKC
		"\u2168":   "IX",     // ROMAN NUMERAL NINE normalises under NFKC
		"a\u00A0b": "a b",    // NO-BREAK SPACE maps to a plain space
		"simple":   "simple", // printable ASCII is the identity
	}
	for in, want := range cases {
		if got := saslprep(in); got != want {
			t.Errorf("saslprep(%q) = %q, want %q", in, got, want)
		}
	}
}

// PostgreSQL falls back to the raw bytes when SASLprep rejects the input, and
// so must we, or the verifier will not match the server's expectation.
func TestSASLprepFallsBackOnProhibitedInput(t *testing.T) {
	in := "bad\u0007control" // BEL is prohibited by RFC 4013
	if got := saslprep(in); got != in {
		t.Fatalf("saslprep(%q) = %q, want the input unchanged", in, got)
	}
}

// A non-ASCII password must produce a verifier over the *prepared* form.
func TestVerifierUsesPreparedPassword(t *testing.T) {
	salt := []byte("0123456789abcdef")
	// These two differ only by a SOFT HYPHEN, which SASLprep removes, so both
	// must yield the same verifier.
	a := ScramVerifier(saslprep("I\u00ADX"), salt, scramIterations)
	b := ScramVerifier(saslprep("IX"), salt, scramIterations)
	if a != b {
		t.Fatal("SASLprep was not applied before hashing")
	}
}

// PostgreSQL (RFC 4013), pgx (precis.OpaqueString) and this package must all
// derive the same key. Handing pgx a pre-prepared password is what makes that
// true; the property it relies on is that SASLprep output is a fixed point.
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

// The connection URL must carry the prepared form, or pgx and the server derive
// different keys for a non-ASCII password.
func TestLibpqURLCarriesPreparedPassword(t *testing.T) {
	withEnv(t, baseEnv())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// SOFT HYPHEN is removed by SASLprep, so both must yield the same URL.
	if a, b := c.LibpqURL("u", "I­X", "db"), c.LibpqURL("u", "IX", "db"); a != b {
		t.Fatalf("URL not built from the prepared password:\n%s\n%s", a, b)
	}
}
