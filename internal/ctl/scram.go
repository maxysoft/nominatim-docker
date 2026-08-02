package ctl

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/xdg-go/stringprep"
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
	return QuoteLiteral(ScramVerifier(saslprep(password), salt, scramIterations)), nil
}
