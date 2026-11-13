package pgclient

import "strings"

// markerPrefix starts the ownership marker kept in COMMENT ON ROLE and
// COMMENT ON DATABASE (ADR 0002 section 10).
const markerPrefix = "craftainer.dev/claim="

// Marker is the comment on a backend object: the owning claim as
// <namespace>/<name>/<uid>, and for a role the privileges it was last given and
// the login generation that is active.
type Marker struct {
	Claim      string
	Privileges string
	Active     string
}

// String renders the comment; empty fields are left out.
func (m Marker) String() string {
	s := markerPrefix + m.Claim
	if m.Privileges != "" {
		s += " privileges=" + m.Privileges
	}
	if m.Active != "" {
		s += " active=" + m.Active
	}
	return s
}

// ParseMarker reads a comment. A comment that is not a marker yields a Claim
// that matches no claim, so the object is refused rather than adopted; an
// empty comment yields the zero Marker.
func ParseMarker(s string) Marker {
	var m Marker
	for i, f := range strings.Fields(s) {
		switch {
		case i == 0:
			m.Claim = strings.TrimPrefix(f, markerPrefix)
		case strings.HasPrefix(f, "privileges="):
			m.Privileges = strings.TrimPrefix(f, "privileges=")
		case strings.HasPrefix(f, "active="):
			m.Active = strings.TrimPrefix(f, "active=")
		}
	}
	return m
}
