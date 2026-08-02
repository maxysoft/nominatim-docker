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

// The whole point: the cleartext password must not appear in the statement.
func TestPasswordSecretHidesTheCleartext(t *testing.T) {
	const pw = "s3cret-password"
	sql, hashed, err := passwordSecret(pw)
	if err != nil {
		t.Fatalf("passwordSecret: %v", err)
	}
	if !hashed {
		t.Fatal("a printable-ASCII password should be sent as a verifier")
	}
	if strings.Contains(sql, pw) {
		t.Fatalf("cleartext password present in the statement: %s", sql)
	}
	if !strings.HasPrefix(sql, "'SCRAM-SHA-256$4096:") {
		t.Fatalf("unexpected verifier shape: %s", sql)
	}
}

// A fresh salt per call means two invocations differ even for one password.
func TestPasswordSecretSaltsEachCall(t *testing.T) {
	a, _, err := passwordSecret("pw")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := passwordSecret("pw")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("expected a fresh random salt on each call")
	}
}

// SASLprep is the identity only for printable ASCII. Anything else is sent as
// cleartext so the server can normalise it, rather than silently producing a
// verifier that will not authenticate.
func TestPasswordSecretFallsBackForNonASCII(t *testing.T) {
	for _, pw := range []string{"pässwort", "pass\tword", "naïve", "emoji🙂"} {
		sql, hashed, err := passwordSecret(pw)
		if err != nil {
			t.Fatalf("passwordSecret(%q): %v", pw, err)
		}
		if hashed {
			t.Errorf("password %q should not be pre-hashed", pw)
		}
		if !strings.Contains(sql, pw) {
			t.Errorf("fallback for %q did not send the cleartext: %s", pw, sql)
		}
	}
}

func TestIsPrintableASCII(t *testing.T) {
	cases := map[string]bool{
		"simple":       true,
		"with space":   true,
		"~!@#$%^&*()_": true,
		"":             true,
		"tab\there":    false,
		"nl\nhere":     false,
		"café":         false,
	}
	for in, want := range cases {
		if got := isPrintableASCII(in); got != want {
			t.Errorf("isPrintableASCII(%q) = %v, want %v", in, got, want)
		}
	}
}
