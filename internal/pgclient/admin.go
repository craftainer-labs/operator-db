package pgclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-db/internal/egress"
)

var (
	// ErrNoDatabase means a connection named a database that does not exist
	// (SQLSTATE 3D000). It is not an unreachable server.
	ErrNoDatabase = errors.New("database does not exist")
	// ErrInvalidPrivileges means a DatabaseRole asked for unknown privileges.
	ErrInvalidPrivileges = errors.New("invalid privileges")
	// ErrNotOwned means a role of a DatabaseRole exists and its ownership
	// marker does not name the claim: the operator never touches it.
	ErrNotOwned = errors.New("role belongs to another claim or is not managed by this operator")
)

// The categories of a failed connection. A condition and an Event carry the
// error text to the tenant, and a tenant can aim a namespaced DatabaseServer at
// any host, so they carry only one of these fixed texts: never an address, a
// resolved name or the raw error, which would turn the operator into a port
// scanner and a resolver. The raw error goes to the operator log.
var (
	ErrConnPolicy  = errors.New("connection refused by the egress policy")
	ErrConnTLS     = errors.New("TLS handshake failed")
	ErrConnAuth    = errors.New("authentication failed")
	ErrConnTimeout = errors.New("connection timed out")
	ErrConnFailed  = errors.New("connection failed")
)

// connError is lifecycle.ErrUnreachable with a category as its text. It still
// unwraps to the cause, for errors.Is, but Error never shows it.
type connError struct{ category, cause error }

func (e *connError) Error() string {
	return lifecycle.ErrUnreachable.Error() + ": " + e.category.Error()
}

func (e *connError) Unwrap() []error {
	return []error{lifecycle.ErrUnreachable, e.category, e.cause}
}

// tlsText reports whether the text of a connection error is one of pgconn's and
// crypto/tls's TLS failures ("tls error", "tls: ...", "x509: ..."), not just a
// host name that contains "tls".
func tlsText(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "tls error") || strings.Contains(msg, "tls: ") || strings.Contains(msg, "x509: ")
}

// classify maps a connection failure to its category.
func classify(err error) error {
	var pgErr *pgconn.PgError
	var verify *tls.CertificateVerificationError
	var netErr net.Error
	switch {
	case errors.Is(err, egress.ErrBlocked):
		return ErrConnPolicy
	case errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "28"):
		return ErrConnAuth
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return ErrConnTimeout
	case errors.As(err, &verify) || tlsText(err.Error()):
		return ErrConnTLS
	}
	return ErrConnFailed
}

// sqlStateInvalidCatalog is SQLSTATE 3D000, invalid_catalog_name.
const sqlStateInvalidCatalog = "3D000"

// OpenFunc opens a Session on a database of the server; "" is the server's
// admin database.
type OpenFunc func(ctx context.Context, database string) (Session, error)

// Admin runs the operator's SQL with the server's admin credentials.
type Admin struct{ Open OpenFunc }

// open wraps a connection failure in lifecycle.ErrUnreachable, so Reconcile
// reports ServerReachable=False, except for a missing database. The text of
// the wrapper is one of the Err Conn categories; the raw error is logged.
func (a Admin) open(ctx context.Context, database string) (Session, error) {
	s, err := a.Open(ctx, database)
	if err == nil {
		return s, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlStateInvalidCatalog {
		return nil, fmt.Errorf("%w: %q", ErrNoDatabase, database)
	}
	log.FromContext(ctx).Error(err, "cannot connect to the backend", "database", database)
	return nil, &connError{category: classify(err), cause: err}
}

// with runs f on a session on database and closes it.
func (a Admin) with(ctx context.Context, database string, f func(Session) error) error {
	s, err := a.open(ctx, database)
	if err != nil {
		return err
	}
	defer s.Close(ctx)
	return f(s)
}

func execAll(ctx context.Context, s Session, stmts []string) error {
	for _, q := range stmts {
		if err := s.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func atoi(s string) int32 {
	n, _ := strconv.ParseInt(s, 10, 32) // a server integer, always valid
	return int32(n)
}

// DatabaseState is what Observe reads about a database.
type DatabaseState struct {
	Exists     bool
	Marker     Marker
	ConnLimit  int32
	Extensions []string
}

// Database reads the database name and its installed extensions.
func (a Admin) Database(ctx context.Context, name string) (DatabaseState, error) {
	var st DatabaseState
	err := a.with(ctx, "", func(s Session) error {
		rows, err := s.Query(ctx, "SELECT coalesce(shobj_description(oid, 'pg_database'), '') AS marker, "+
			"datconnlimit::text AS connlimit FROM pg_database WHERE datname = "+Literal(name))
		if err != nil || len(rows) == 0 {
			return err
		}
		st = DatabaseState{Exists: true, Marker: ParseMarker(rows[0]["marker"]), ConnLimit: atoi(rows[0]["connlimit"])}
		return nil
	})
	if err != nil || !st.Exists {
		return st, err
	}
	err = a.with(ctx, name, func(s Session) error {
		rows, err := s.Query(ctx, "SELECT extname FROM pg_extension ORDER BY extname")
		for _, r := range rows {
			st.Extensions = append(st.Extensions, r["extname"])
		}
		return err
	})
	return st, err
}

// DatabaseParams are the settings of a database.
type DatabaseParams struct {
	Name       string
	Marker     Marker
	Locale     string
	ConnLimit  int32
	Extensions []string
}

// CreateDatabase creates the database from template0 (so the locale is free
// to differ from template1's), stamps the marker and creates the extensions.
func (a Admin) CreateDatabase(ctx context.Context, p DatabaseParams) error {
	if err := ValidateExtensions(p.Extensions); err != nil {
		return err
	}
	q := "CREATE DATABASE " + Ident(p.Name) + " TEMPLATE template0 ENCODING 'UTF8'"
	if p.Locale != "" {
		q += " LOCALE " + Literal(p.Locale)
	}
	q += " CONNECTION LIMIT " + strconv.Itoa(int(p.ConnLimit))
	err := a.with(ctx, "", func(s Session) error {
		return execAll(ctx, s, []string{q, commentOn("DATABASE", p.Name, p.Marker)})
	})
	if err != nil {
		return err
	}
	return a.ensureExtensions(ctx, p)
}

// UpdateDatabase makes the connection limit and the marker match and creates
// the missing extensions. It never drops an extension: that could drop data.
func (a Admin) UpdateDatabase(ctx context.Context, p DatabaseParams) error {
	if err := ValidateExtensions(p.Extensions); err != nil {
		return err
	}
	err := a.with(ctx, "", func(s Session) error {
		return execAll(ctx, s, []string{
			"ALTER DATABASE " + Ident(p.Name) + " CONNECTION LIMIT " + strconv.Itoa(int(p.ConnLimit)),
			commentOn("DATABASE", p.Name, p.Marker),
		})
	})
	if err != nil {
		return err
	}
	return a.ensureExtensions(ctx, p)
}

func (a Admin) ensureExtensions(ctx context.Context, p DatabaseParams) error {
	if len(p.Extensions) == 0 {
		return nil
	}
	return a.with(ctx, p.Name, func(s Session) error {
		for _, e := range p.Extensions {
			if err := s.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+Ident(e)); err != nil {
				return err
			}
		}
		return nil
	})
}

// DropDatabase removes the database and disconnects its sessions.
func (a Admin) DropDatabase(ctx context.Context, name string) error {
	return a.with(ctx, "", func(s Session) error {
		return s.Exec(ctx, "DROP DATABASE IF EXISTS "+Ident(name)+" WITH (FORCE)")
	})
}

func commentOn(kind, name string, m Marker) string {
	return "COMMENT ON " + kind + " " + Ident(name) + " IS " + Literal(m.String())
}

// RoleInfo is one role as Observe reads it.
type RoleInfo struct {
	Exists    bool
	Login     bool
	ConnLimit int32
	// Marker is the ownership marker in the role's comment; the zero Marker
	// when it has none.
	Marker Marker
}

// RoleState is the owner role and both logins of a DatabaseRole.
type RoleState struct {
	Base, A, B RoleInfo
	Marker     Marker
}

// Login returns the login role of a generation.
func (s RoleState) Login(gen string) RoleInfo {
	if gen == GenB {
		return s.B
	}
	return s.A
}

// Role reads the owner role and the two logins.
func (a Admin) Role(ctx context.Context, base string) (RoleState, error) {
	var st RoleState
	err := a.with(ctx, "", func(s Session) error {
		names := []string{Literal(base), Literal(LoginName(base, GenA)), Literal(LoginName(base, GenB))}
		rows, err := s.Query(ctx, "SELECT rolname AS name, rolcanlogin::text AS login, rolconnlimit::text AS connlimit, "+
			"coalesce(shobj_description(oid, 'pg_authid'), '') AS comment FROM pg_roles WHERE rolname IN ("+
			strings.Join(names, ", ")+")")
		for _, r := range rows {
			info := RoleInfo{Exists: true, Login: r["login"] == "true", ConnLimit: atoi(r["connlimit"]), Marker: ParseMarker(r["comment"])}
			switch r["name"] {
			case base:
				st.Base, st.Marker = info, info.Marker
			case LoginName(base, GenA):
				st.A = info
			case LoginName(base, GenB):
				st.B = info
			}
		}
		return err
	})
	return st, err
}

// RoleParams are the settings of a DatabaseRole's roles. Marker.Active names
// the login that gets Password.
type RoleParams struct {
	Base       string
	Database   string
	Marker     Marker
	ConnLimit  int32
	Password   string
	Privileges string
}

// Privileges of a DatabaseRole, the values of spec.privileges.
const (
	ReadOnly  = "ReadOnly"
	ReadWrite = "ReadWrite"
	Owner     = "Owner"
)

// ApplyRole creates what is missing and makes the rest match: the NOLOGIN
// owner role, both logins as its members with SET ROLE to it, the active
// login's password, the privileges on the database and, last, the marker. The
// other login keeps its state, because it may be the previous generation of a
// rotation.
//
// A role is created together with its ownership marker (a COMMENT with the
// claim only) in one multi-statement query, which PostgreSQL runs as one
// transaction, so a role without a marker is never ours. An existing role
// whose marker names another claim is refused. The full marker, privileges and
// active login included, is written after the grants, so it never records
// privileges that were not applied.
func (a Admin) ApplyRole(ctx context.Context, p RoleParams) error {
	grants, err := privilegeStatements(p.Base, p.Privileges)
	if err != nil {
		return err
	}
	st, err := a.Role(ctx, p.Base)
	if err != nil {
		return err
	}
	for _, r := range []struct {
		name string
		info RoleInfo
	}{{p.Base, st.Base}, {LoginName(p.Base, GenA), st.A}, {LoginName(p.Base, GenB), st.B}} {
		if r.info.Exists && r.info.Marker.Claim != p.Marker.Claim {
			return fmt.Errorf("%w: %q", ErrNotOwned, r.name)
		}
	}
	owned := Marker{Claim: p.Marker.Claim}
	create := func(name string) string {
		return "CREATE ROLE " + Ident(name) + " NOLOGIN; " + commentOn("ROLE", name, owned)
	}
	base, db, limit := Ident(p.Base), Ident(p.Database), strconv.Itoa(int(p.ConnLimit))
	var stmts []string
	if !st.Base.Exists {
		stmts = append(stmts, create(p.Base))
	}
	for _, gen := range []string{GenA, GenB} {
		login := Ident(LoginName(p.Base, gen))
		if !st.Login(gen).Exists {
			stmts = append(stmts, create(LoginName(p.Base, gen)))
		}
		alter := "ALTER ROLE " + login + " WITH CONNECTION LIMIT " + limit
		if gen == p.Marker.Active {
			alter = "ALTER ROLE " + login + " WITH LOGIN PASSWORD " + Literal(p.Password) + " CONNECTION LIMIT " + limit
		}
		stmts = append(stmts, alter, "GRANT "+base+" TO "+login, "ALTER ROLE "+login+" SET role = "+Literal(p.Base))
	}
	access := "CONNECT"
	if p.Privileges == Owner {
		access = "ALL"
	}
	stmts = append(stmts, "REVOKE ALL ON DATABASE "+db+" FROM "+base, "GRANT "+access+" ON DATABASE "+db+" TO "+base)
	if err := a.with(ctx, "", func(s Session) error { return execAll(ctx, s, stmts) }); err != nil {
		return err
	}
	if err := a.with(ctx, p.Database, func(s Session) error { return execAll(ctx, s, grants) }); err != nil {
		return err
	}
	return a.with(ctx, "", func(s Session) error { return s.Exec(ctx, commentOn("ROLE", p.Base, p.Marker)) })
}

// privilegeStatements revoke and then grant the access of a privilege level on
// the public schema of the database the session is connected to. Default
// privileges cover the objects the admin role creates later; objects an Owner
// role creates are covered at the next privilege change (see the README).
func privilegeStatements(base, privileges string) ([]string, error) {
	var schema, tables, seqs string
	switch privileges {
	case ReadOnly:
		schema, tables, seqs = "USAGE", "SELECT", "SELECT"
	case ReadWrite:
		schema, tables, seqs = "USAGE", "SELECT, INSERT, UPDATE, DELETE", "USAGE, SELECT, UPDATE"
	case Owner:
		schema, tables, seqs = "ALL", "ALL", "ALL"
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidPrivileges, privileges)
	}
	r := Ident(base)
	return []string{
		"REVOKE ALL ON ALL TABLES IN SCHEMA public FROM " + r,
		"REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM " + r,
		"REVOKE ALL ON SCHEMA public FROM " + r,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM " + r,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM " + r,
		"GRANT " + schema + " ON SCHEMA public TO " + r,
		"GRANT " + tables + " ON ALL TABLES IN SCHEMA public TO " + r,
		"GRANT " + seqs + " ON ALL SEQUENCES IN SCHEMA public TO " + r,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT " + tables + " ON TABLES TO " + r,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT " + seqs + " ON SEQUENCES TO " + r,
	}, nil
}

// Retire disables the login of gen and clears its password. It succeeds when
// the role is already gone.
func (a Admin) Retire(ctx context.Context, base, gen string) error {
	st, err := a.Role(ctx, base)
	if err != nil || !st.Login(gen).Exists {
		return err
	}
	return a.with(ctx, "", func(s Session) error {
		return s.Exec(ctx, "ALTER ROLE "+Ident(LoginName(base, gen))+" WITH NOLOGIN PASSWORD NULL")
	})
}

// DropRole ends the sessions of the roles that exist, hands the objects they
// own to the admin role and drops them. database may be gone already.
func (a Admin) DropRole(ctx context.Context, base, database string) error {
	st, err := a.Role(ctx, base)
	if err != nil {
		return err
	}
	var idents, literals []string
	for name, exists := range map[string]bool{
		base: st.Base.Exists, LoginName(base, GenA): st.A.Exists, LoginName(base, GenB): st.B.Exists,
	} {
		if exists {
			idents, literals = append(idents, Ident(name)), append(literals, Literal(name))
		}
	}
	if len(idents) == 0 {
		return nil
	}
	slices.Sort(idents)
	slices.Sort(literals)
	list := strings.Join(idents, ", ")
	err = a.with(ctx, "", func(s Session) error {
		_, err := s.Query(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename IN ("+
			strings.Join(literals, ", ")+")")
		return err
	})
	if err != nil {
		return err
	}
	err = a.with(ctx, database, func(s Session) error {
		return execAll(ctx, s, []string{"REASSIGN OWNED BY " + list + " TO CURRENT_USER", "DROP OWNED BY " + list})
	})
	if err != nil && !errors.Is(err, ErrNoDatabase) {
		return err
	}
	return a.with(ctx, "", func(s Session) error {
		return s.Exec(ctx, "DROP ROLE IF EXISTS "+list)
	})
}
