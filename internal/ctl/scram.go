package ctl

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/pbkdf2"
)

// scramIterations is PostgreSQL's own default for password_encryption.
const scramIterations = 4096

// ScramVerifier builds the value PostgreSQL stores in pg_authid.rolpassword.
//
// `ALTER ROLE x PASSWORD 'cleartext'` sends the password to the server, which
// logs it verbatim when log_statement is 'ddl' or 'all' — on a managed provider
// that log is often shipped to a shared sink and retained for months. Computing
// the verifier here sends only a salted hash, which is what the server would
// have derived and stored anyway.
//
// Format: SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>, per
// RFC 5802 with PostgreSQL's encoding.
func ScramVerifier(password string, salt []byte, iterations int) string {
	saltedPassword := pbkdf2.Key([]byte(password), salt, iterations, sha256.Size, sha256.New)

	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))

	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		iterations, b64(salt), b64(storedKey[:]), b64(serverKey))
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// passwordSecret renders the value for a CREATE/ALTER ROLE ... PASSWORD clause.
//
// PostgreSQL applies SASLprep (RFC 4013) to a cleartext password before hashing
// it. SASLprep is the identity mapping for printable ASCII, so a verifier
// computed here matches the server's for those passwords. For anything else we
// send the cleartext and let the server normalise it — a correct login matters
// more than keeping it out of a log the operator may not even have enabled.
func passwordSecret(password string) (sql string, hashed bool, err error) {
	if !isPrintableASCII(password) {
		return QuoteLiteral(password), false, nil
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", false, fmt.Errorf("generating SCRAM salt: %w", err)
	}
	return QuoteLiteral(ScramVerifier(password, salt, scramIterations)), true, nil
}

// isPrintableASCII reports whether every byte is in the range SASLprep leaves
// untouched (space through tilde).
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
