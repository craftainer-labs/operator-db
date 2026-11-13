package pgclient

import (
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/naming"
)

func TestQuoting(t *testing.T) {
	for in, want := range map[string]string{
		"orders":           `"orders"`,
		"team-a_orders":    `"team-a_orders"`,
		`we"ird`:           `"we""ird"`,
		"a\x00b":           `"ab"`,
		`x"; DROP TABLE y`: `"x""; DROP TABLE y"`,
	} {
		if got := Ident(in); got != want {
			t.Errorf("Ident(%q) = %s, want %s", in, got, want)
		}
	}
	if got := Ident("public", "t"); got != `"public"."t"` {
		t.Errorf("Ident(public, t) = %s", got)
	}
	for in, want := range map[string]string{
		"pw":        `'pw'`,
		"it's":      `'it''s'`,
		`a\b`:       `'a\b'`,
		"a\x00b":    `'ab'`,
		"'; DROP--": `'''; DROP--'`,
	} {
		if got := Literal(in); got != want {
			t.Errorf("Literal(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestNamesUseTheHashLengthOfADR0002(t *testing.T) {
	// Section 10: databases and roles can hold the ownership marker, so they use
	// the short hash; login roles leave room for _a and _b.
	db := DatabaseName("team-a", "orders")
	if db != naming.Tenant("team-a", "orders", 63, "_", naming.HashShort) || db != "team-a_orders_gdeqxvirep" {
		t.Errorf("DatabaseName = %q", db)
	}
	long := strings.Repeat("a", 60)
	if n := len(DatabaseName("team-a", long)); n != 63 {
		t.Errorf("long database name is %d bytes", n)
	}
	base := RoleBase("team-a", long)
	if len(base) != 61 || len(LoginName(base, GenA)) != 63 || LoginName(base, GenB) != base+"_b" {
		t.Errorf("RoleBase = %q (%d)", base, len(base))
	}
	if OtherGen(GenA) != GenB || OtherGen(GenB) != GenA || OtherGen("") != GenB {
		t.Error("OtherGen")
	}
}

func TestValidateExtensions(t *testing.T) {
	if err := ValidateExtensions([]string{"pgcrypto", "uuid-ossp", "_x", "a1"}); err != nil {
		t.Error(err)
	}
	if err := ValidateExtensions(nil); err != nil {
		t.Error(err)
	}
}

func TestMarker(t *testing.T) {
	m := Marker{Claim: "ns/n/uid", Privileges: "Owner", Active: "b"}
	if got := m.String(); got != "craftainer.dev/claim=ns/n/uid privileges=Owner active=b" {
		t.Errorf("String = %q", got)
	}
	if ParseMarker(m.String()) != m {
		t.Errorf("round trip = %+v", ParseMarker(m.String()))
	}
	if got := (Marker{Claim: "c"}).String(); got != "craftainer.dev/claim=c" {
		t.Errorf("claim only = %q", got)
	}
	if (ParseMarker("") != Marker{}) {
		t.Error("an empty comment is the zero Marker")
	}
	// A foreign comment never equals a claim's id.
	if ParseMarker("hand written note").Claim == "ns/n/uid" {
		t.Error("foreign comment parsed as a claim")
	}
	if ParseMarker("craftainer.dev/claim=x unknown=1 active=a").Active != "a" {
		t.Error("unknown fields are skipped")
	}
}
