package pgclient

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strconv"
)

// ErrPasswordNotASCII means a password cannot be turned into a SCRAM verifier
// here: the operator only issues ASCII passwords, for which SASLprep (RFC 4013)
// is the identity, so anything else is refused rather than normalised wrongly.
var ErrPasswordNotASCII = errors.New("the password must be printable ASCII")

// scramIterations is PostgreSQL's default scram_iterations.
const scramIterations = 4096

// saltSource is where verifier salts come from; a test replaces it.
var saltSource io.Reader = rand.Reader

// ScramVerifier computes the SCRAM-SHA-256 secret PostgreSQL stores for a
// password (RFC 5802, 7677): SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>.
// Sending it in CREATE/ALTER ROLE ... PASSWORD means the password itself never
// reaches the server, its statement log or pg_stat_statements.
func ScramVerifier(password string) (string, error) {
	for _, r := range password {
		if r < 0x20 || r > 0x7e {
			return "", ErrPasswordNotASCII
		}
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(saltSource, salt); err != nil {
		return "", err
	}
	return scramVerifier(password, salt, scramIterations), nil
}

func scramVerifier(password string, salt []byte, iterations int) string {
	mac := func(key []byte, msg ...[]byte) []byte {
		h := hmac.New(sha256.New, key)
		for _, m := range msg {
			_, _ = h.Write(m)
		}
		return h.Sum(nil)
	}
	// Hi(password, salt, i) of RFC 5802 is PBKDF2-HMAC-SHA-256 with a 32-byte
	// output, which is the single block 1; written out here because the
	// standard library's version can fail in FIPS mode.
	u := mac([]byte(password), salt, []byte{0, 0, 0, 1})
	salted := bytes.Clone(u)
	for i := 1; i < iterations; i++ {
		u = mac([]byte(password), u)
		for j := range salted {
			salted[j] ^= u[j]
		}
	}
	stored := sha256.Sum256(mac(salted, []byte("Client Key")))
	enc := base64.StdEncoding.EncodeToString
	return "SCRAM-SHA-256$" + strconv.Itoa(iterations) + ":" + enc(salt) + "$" + enc(stored[:]) + ":" + enc(mac(salted, []byte("Server Key")))
}
