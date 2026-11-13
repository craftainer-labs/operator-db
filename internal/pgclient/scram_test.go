package pgclient

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// The RFC 7677 example: password "pencil", salt W22ZaJ0SNY7soEsUEjb6gQ==, 4096
// iterations. The expected keys were computed independently with Python's
// hashlib.pbkdf2_hmac and hmac.
func TestScramVerifierVector(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	got := scramVerifier("pencil", salt, 4096)
	want := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
	if got != want {
		t.Errorf("verifier = %q", got)
	}
}

func TestScramVerifierSaltAndInput(t *testing.T) {
	old := saltSource
	t.Cleanup(func() { saltSource = old })

	saltSource = bytes.NewReader(bytes.Repeat([]byte{7}, 16))
	v, err := ScramVerifier("p'w")
	if err != nil || !strings.HasPrefix(v, "SCRAM-SHA-256$4096:") || strings.Contains(v, "p'w") {
		t.Errorf("verifier = %q, %v", v, err)
	}
	// Every call draws a fresh salt from the real source.
	saltSource = old
	a, _ := ScramVerifier("same")
	b, _ := ScramVerifier("same")
	if a == b {
		t.Error("two verifiers of one password share a salt")
	}

	if _, err := ScramVerifier("pässword"); !errors.Is(err, ErrPasswordNotASCII) {
		t.Errorf("non-ASCII: %v", err)
	}
	if _, err := ScramVerifier("tab\tbed"); !errors.Is(err, ErrPasswordNotASCII) {
		t.Errorf("control character: %v", err)
	}
	saltSource = bytes.NewReader(nil)
	if _, err := ScramVerifier("x"); err == nil {
		t.Error("an exhausted salt source succeeded")
	}
}
