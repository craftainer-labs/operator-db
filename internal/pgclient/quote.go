package pgclient

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/craftainer-labs/lib-operator-go/pkg/naming"
)

// Ident quotes SQL identifiers (a database, a role, an extension) with
// pgx.Identifier, which double-quotes every part and doubles embedded quotes.
// ADR 0002 section 10: names derived from a claim contain '-', legal only
// quoted, so no identifier is ever written bare.
func Ident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

// Literal quotes a string literal. The session sets standard_conforming_strings
// on, so a backslash is an ordinary character and doubling the quote is the
// whole escape. NUL bytes cannot occur in a PostgreSQL string and are dropped.
func Literal(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\x00", ""), "'", "''") + "'"
}

// extensionPattern is the CRD pattern of extension names (user decision 4),
// checked here as well because the controller must not rely on admission.
var extensionPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

// ErrInvalidExtension means an extension name is not an identifier the CRD
// would admit.
var ErrInvalidExtension = errors.New("invalid extension name")

// ValidateExtensions checks names against the CRD pattern and the identifier
// limit of 63 bytes.
func ValidateExtensions(names []string) error {
	for _, n := range names {
		if len(n) > 63 || !extensionPattern.MatchString(n) {
			return fmt.Errorf("%w: %q", ErrInvalidExtension, n)
		}
	}
	return nil
}

// Logins of a DatabaseRole, besides its NOLOGIN owner role (ADR 0002 section 8).
const (
	GenA = "a"
	GenB = "b"
)

// DatabaseName is the PostgreSQL database of a Database claim. The ownership
// marker is the second barrier, so the 50-bit hash is enough (section 10).
func DatabaseName(ns, name string) string {
	return naming.Tenant(ns, name, 63, "_", naming.HashShort)
}

// RoleBase is the NOLOGIN owner role of a DatabaseRole; its logins add _a and
// _b, so it leaves two bytes of the 63-byte limit.
func RoleBase(ns, name string) string {
	return naming.Tenant(ns, name, 61, "_", naming.HashShort)
}

// LoginName is the login role of a generation.
func LoginName(base, gen string) string { return base + "_" + gen }

// OtherGen is the generation that is not gen; "" counts as GenA, the one
// Create issues.
func OtherGen(gen string) string {
	if gen == GenB {
		return GenA
	}
	return GenB
}
