package pgclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
)

var errBoom = errors.New("boom")

type scripted struct {
	sub  string
	rows []Row
}

// fakePG records every statement as "<database>|<sql>" and answers queries by
// substring.
type fakePG struct {
	stmts   []string
	closed  int
	rows    []scripted
	failOn  string
	failErr error // returned instead of errBoom
	openErr map[string]error
}

func (f *fakePG) admin() Admin { return Admin{Open: f.open} }

func (f *fakePG) open(_ context.Context, db string) (Session, error) {
	if err := f.openErr[db]; err != nil {
		return nil, err
	}
	return &fakeSession{f: f, db: db}, nil
}

type fakeSession struct {
	f  *fakePG
	db string
}

func (s *fakeSession) record(sql string) error {
	s.f.stmts = append(s.f.stmts, s.db+"|"+sql)
	if s.f.failOn != "" && strings.Contains(sql, s.f.failOn) {
		if s.f.failErr != nil {
			return s.f.failErr
		}
		return errBoom
	}
	return nil
}

func (s *fakeSession) Exec(_ context.Context, sql string) error { return s.record(sql) }

func (s *fakeSession) Query(_ context.Context, sql string) ([]Row, error) {
	if err := s.record(sql); err != nil {
		return nil, err
	}
	for _, sc := range s.f.rows {
		if strings.Contains(sql, sc.sub) {
			return sc.rows, nil
		}
	}
	return nil, nil
}

func (s *fakeSession) Close(context.Context) { s.f.closed++ }

func (f *fakePG) joined() string { return strings.Join(f.stmts, "\n") }

func (f *fakePG) want(t *testing.T, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(f.joined(), sub) {
			t.Errorf("no statement contains %q; statements:\n%s", sub, f.joined())
		}
	}
}

func (f *fakePG) wantNot(t *testing.T, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(f.joined(), sub) {
			t.Errorf("a statement contains %q; statements:\n%s", sub, f.joined())
		}
	}
}

func TestOpenClassifiesFailures(t *testing.T) {
	ctx := context.Background()
	f := &fakePG{openErr: map[string]error{"": errBoom, "gone": &pgconn.PgError{Code: "3D000"}}}
	if _, err := f.admin().Database(ctx, "x"); !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, errBoom) {
		t.Errorf("connect failure: %v", err)
	}
	f.openErr = map[string]error{"gone": &pgconn.PgError{Code: "3D000"}, "other": &pgconn.PgError{Code: "28P01"}}
	if err := f.admin().with(ctx, "gone", func(Session) error { return nil }); !errors.Is(err, ErrNoDatabase) || errors.Is(err, lifecycle.ErrUnreachable) {
		t.Errorf("missing database: %v", err)
	}
	if err := f.admin().with(ctx, "other", func(Session) error { return nil }); !errors.Is(err, lifecycle.ErrUnreachable) {
		t.Errorf("auth failure: %v", err)
	}
}

func TestDatabaseObserve(t *testing.T) {
	ctx := context.Background()
	f := &fakePG{}
	st, err := f.admin().Database(ctx, "team-a_orders_x")
	if err != nil || st.Exists {
		t.Errorf("missing database = %+v, %v", st, err)
	}
	f.want(t, `datname = 'team-a_orders_x'`)

	f = &fakePG{rows: []scripted{
		{"FROM pg_database", []Row{{"marker": "craftainer.dev/claim=team-a/orders/uid-1", "connlimit": "5"}}},
		{"FROM pg_extension", []Row{{"extname": "pgcrypto"}, {"extname": "plpgsql"}}},
	}}
	st, err = f.admin().Database(ctx, "d")
	if err != nil || !st.Exists || st.Marker.Claim != "team-a/orders/uid-1" || st.ConnLimit != 5 || !slices.Equal(st.Extensions, []string{"pgcrypto", "plpgsql"}) {
		t.Errorf("state = %+v, %v", st, err)
	}
	if f.closed != 2 {
		t.Errorf("closed %d sessions, want 2", f.closed)
	}

	f.failOn = "pg_database"
	if _, err := f.admin().Database(ctx, "d"); !errors.Is(err, errBoom) {
		t.Errorf("query error: %v", err)
	}
	f.failOn = "pg_extension"
	if _, err := f.admin().Database(ctx, "d"); !errors.Is(err, errBoom) {
		t.Errorf("extension query error: %v", err)
	}
	f.failOn, f.openErr = "", map[string]error{"d": errBoom}
	if _, err := f.admin().Database(ctx, "d"); !errors.Is(err, errBoom) {
		t.Errorf("open error: %v", err)
	}
}

func TestCreateUpdateDropDatabase(t *testing.T) {
	ctx := context.Background()
	p := DatabaseParams{
		Name: `team-a_o"rders_x`, Marker: Marker{Claim: "team-a/orders/uid-1"}, Locale: "de_DE.UTF-8",
		ConnLimit: -1, Extensions: []string{"pgcrypto", "uuid-ossp"},
	}
	f := &fakePG{}
	if err := f.admin().CreateDatabase(ctx, p); err != nil {
		t.Fatal(err)
	}
	f.want(t,
		`|CREATE DATABASE "team-a_o""rders_x" TEMPLATE template0 ENCODING 'UTF8' LOCALE 'de_DE.UTF-8' CONNECTION LIMIT -1`,
		`|COMMENT ON DATABASE "team-a_o""rders_x" IS 'craftainer.dev/claim=team-a/orders/uid-1'`,
		`team-a_o"rders_x|CREATE EXTENSION IF NOT EXISTS "pgcrypto"`,
		`team-a_o"rders_x|CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`)

	// Without a locale or extensions the database is not even connected to.
	f = &fakePG{}
	p2 := DatabaseParams{Name: "d", ConnLimit: 3}
	if err := f.admin().CreateDatabase(ctx, p2); err != nil {
		t.Fatal(err)
	}
	f.wantNot(t, "LOCALE", "CREATE EXTENSION")
	if f.closed != 1 {
		t.Errorf("sessions = %d, want 1", f.closed)
	}

	// An extension name that is not an identifier is refused before any SQL.
	f = &fakePG{}
	for _, bad := range []string{`x"; DROP DATABASE y; --`, "Upper", "1x", "", strings.Repeat("a", 64)} {
		bp := DatabaseParams{Name: "d", Extensions: []string{bad}}
		if err := f.admin().CreateDatabase(ctx, bp); !errors.Is(err, ErrInvalidExtension) {
			t.Errorf("create %q: %v", bad, err)
		}
		if err := f.admin().UpdateDatabase(ctx, bp); !errors.Is(err, ErrInvalidExtension) {
			t.Errorf("update %q: %v", bad, err)
		}
	}
	if len(f.stmts) != 0 {
		t.Errorf("statements ran: %v", f.stmts)
	}

	f = &fakePG{}
	if err := f.admin().UpdateDatabase(ctx, p); err != nil {
		t.Fatal(err)
	}
	f.want(t, `|ALTER DATABASE "team-a_o""rders_x" CONNECTION LIMIT -1`, `|COMMENT ON DATABASE`, `CREATE EXTENSION IF NOT EXISTS "pgcrypto"`)

	f = &fakePG{}
	if err := f.admin().DropDatabase(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	f.want(t, `|DROP DATABASE IF EXISTS "d" WITH (FORCE)`)

	for _, fail := range []string{"CREATE DATABASE", "COMMENT ON", "CREATE EXTENSION", "ALTER DATABASE", "DROP DATABASE"} {
		f = &fakePG{failOn: fail}
		a := f.admin()
		errs := []error{a.CreateDatabase(ctx, p), a.UpdateDatabase(ctx, p), a.DropDatabase(ctx, "d")}
		if !slices.ContainsFunc(errs, func(e error) bool { return errors.Is(e, errBoom) }) {
			t.Errorf("failOn %q: no error: %v", fail, errs)
		}
	}
	f = &fakePG{openErr: map[string]error{p.Name: errBoom}}
	if err := f.admin().CreateDatabase(ctx, p); !errors.Is(err, errBoom) {
		t.Errorf("extension connect: %v", err)
	}
}

func roleRows(base string, login bool) []scripted {
	l := "false"
	if login {
		l = "true"
	}
	return []scripted{{"FROM pg_roles", []Row{
		{"name": base, "login": "false", "connlimit": "-1", "comment": "craftainer.dev/claim=team-a/app/uid-1 privileges=ReadWrite active=a"},
		{"name": base + "_a", "login": l, "connlimit": "2", "comment": "craftainer.dev/claim=team-a/app/uid-1"},
		{"name": base + "_b", "login": "false", "connlimit": "-1", "comment": "craftainer.dev/claim=team-a/app/uid-1"},
		{"name": "unrelated", "login": "false", "connlimit": "-1"},
	}}}
}

func TestRoleObserve(t *testing.T) {
	ctx := context.Background()
	f := &fakePG{rows: roleRows("r", true)}
	st, err := f.admin().Role(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Base.Exists || st.Base.Login || !st.A.Exists || !st.A.Login || st.A.ConnLimit != 2 || !st.B.Exists || st.B.Login {
		t.Errorf("state = %+v", st)
	}
	if st.Marker != (Marker{Claim: "team-a/app/uid-1", Privileges: "ReadWrite", Active: "a"}) {
		t.Errorf("marker = %+v", st.Marker)
	}
	if st.A.Marker.Claim != "team-a/app/uid-1" || st.B.Marker.Claim != "team-a/app/uid-1" || st.Base.Marker != st.Marker {
		t.Errorf("login markers = %+v, %+v", st.A.Marker, st.B.Marker)
	}
	if st.Login(GenA) != st.A || st.Login(GenB) != st.B {
		t.Error("Login picks the wrong role")
	}
	f.want(t, `rolname IN ('r', 'r_a', 'r_b')`, `g.rolname = 'rds_iam'`)
	if st.Base.RDSIAM || st.A.RDSIAM {
		t.Errorf("rds_iam membership read from rows without it: %+v", st)
	}
	f.rows[0].rows[0]["rdsiam"] = "true"
	if st, err = f.admin().Role(ctx, "r"); err != nil || !st.Base.RDSIAM || st.A.RDSIAM {
		t.Errorf("rds_iam member: %+v, %v", st, err)
	}

	f = &fakePG{}
	st, err = f.admin().Role(ctx, "r")
	if err != nil || st.Base.Exists {
		t.Errorf("missing = %+v, %v", st, err)
	}
	f.failOn = "pg_roles"
	if _, err := f.admin().Role(ctx, "r"); !errors.Is(err, errBoom) {
		t.Errorf("query error: %v", err)
	}
}

func TestApplyRole(t *testing.T) {
	ctx := context.Background()
	p := RoleParams{
		Base: "team-a_app_x", Database: "team-a_db_y", Marker: Marker{Claim: "team-a/app/uid-1", Privileges: ReadWrite, Active: GenA},
		ConnLimit: 7, Password: "p'w", Privileges: ReadWrite,
	}
	f := &fakePG{}
	if err := f.admin().ApplyRole(ctx, p); err != nil {
		t.Fatal(err)
	}
	f.want(t,
		`|CREATE ROLE "team-a_app_x" NOLOGIN`,
		`|CREATE ROLE "team-a_app_x_a" NOLOGIN`,
		`|CREATE ROLE "team-a_app_x_b" NOLOGIN`,
		// Every role is created with its ownership marker in the same query.
		`|CREATE ROLE "team-a_app_x" NOLOGIN; COMMENT ON ROLE "team-a_app_x" IS 'craftainer.dev/claim=team-a/app/uid-1'`,
		`|CREATE ROLE "team-a_app_x_a" NOLOGIN; COMMENT ON ROLE "team-a_app_x_a" IS 'craftainer.dev/claim=team-a/app/uid-1'`,
		`|ALTER ROLE "team-a_app_x_a" WITH LOGIN PASSWORD 'SCRAM-SHA-256$4096:`,
		`|ALTER ROLE "team-a_app_x_b" WITH CONNECTION LIMIT 7`,
		`|GRANT "team-a_app_x" TO "team-a_app_x_a"`,
		`|ALTER ROLE "team-a_app_x_b" SET role = 'team-a_app_x'`,
		`|COMMENT ON ROLE "team-a_app_x" IS 'craftainer.dev/claim=team-a/app/uid-1 privileges=ReadWrite active=a'`,
		`|GRANT CONNECT ON DATABASE "team-a_db_y" TO "team-a_app_x"`,
		`team-a_db_y|GRANT USAGE ON SCHEMA public TO "team-a_app_x"`,
		`team-a_db_y|GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO "team-a_app_x"`)
	f.wantNot(t, "GRANT ALL ON DATABASE", "p'w", "p''w")

	// A second login that exists is left alone, and Owner gets ALL.
	f = &fakePG{rows: roleRows(p.Base, true)}
	p.Privileges, p.Marker.Active = Owner, GenB
	if err := f.admin().ApplyRole(ctx, p); err != nil {
		t.Fatal(err)
	}
	f.wantNot(t, "CREATE ROLE")
	f.want(t, `|ALTER ROLE "team-a_app_x_b" WITH LOGIN PASSWORD`, `|ALTER ROLE "team-a_app_x_a" WITH CONNECTION LIMIT 7`,
		`GRANT ALL ON DATABASE "team-a_db_y" TO "team-a_app_x"`, `GRANT ALL ON SCHEMA public`)

	for _, priv := range []string{ReadOnly, ReadWrite, Owner} {
		stmts, err := privilegeStatements("b", priv)
		if err != nil || len(stmts) != 10 {
			t.Errorf("%s: %v, %d statements", priv, err, len(stmts))
		}
	}
	bad := p
	bad.Privileges = "Root"
	if err := (&fakePG{}).admin().ApplyRole(ctx, bad); !errors.Is(err, ErrInvalidPrivileges) {
		t.Errorf("unknown privileges: %v", err)
	}

	f = &fakePG{failOn: "pg_roles"}
	if err := f.admin().ApplyRole(ctx, p); !errors.Is(err, errBoom) {
		t.Errorf("observe failure: %v", err)
	}
	// A password that has no verifier is refused before any statement.
	f = &fakePG{}
	bad = p
	bad.Password = "pässword"
	if err := f.admin().ApplyRole(ctx, bad); !errors.Is(err, ErrPasswordNotASCII) || len(f.stmts) != 0 {
		t.Errorf("non-ASCII password: %v, %v", err, f.stmts)
	}
	f = &fakePG{failOn: "CREATE ROLE"}
	if err := f.admin().ApplyRole(ctx, p); !errors.Is(err, errBoom) {
		t.Errorf("role failure: %v", err)
	}
	f = &fakePG{failOn: "ON SCHEMA public"}
	if err := f.admin().ApplyRole(ctx, p); !errors.Is(err, errBoom) {
		t.Errorf("grant failure: %v", err)
	}
}

func TestRetire(t *testing.T) {
	ctx := context.Background()
	f := &fakePG{rows: roleRows("r", true)}
	if err := f.admin().Retire(ctx, "r", GenA); err != nil {
		t.Fatal(err)
	}
	f.want(t, `|ALTER ROLE "r_a" WITH NOLOGIN PASSWORD NULL`,
		`|SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = 'r_a'`)

	// A login that is already gone is fine.
	f = &fakePG{}
	if err := f.admin().Retire(ctx, "r", GenA); err != nil {
		t.Fatal(err)
	}
	f.wantNot(t, "ALTER ROLE")
	for _, fail := range []string{"pg_roles", "NOLOGIN", "pg_terminate_backend"} {
		f = &fakePG{rows: roleRows("r", true), failOn: fail}
		if err := f.admin().Retire(ctx, "r", GenA); !errors.Is(err, errBoom) {
			t.Errorf("failOn %q: %v", fail, err)
		}
	}

	// An admin that may not terminate sessions has still retired the login; the
	// error says so. Other server errors on the terminate query still fail.
	f = &fakePG{rows: roleRows("r", true), failOn: "pg_terminate_backend", failErr: &pgconn.PgError{Code: "42501"}}
	if err := f.admin().Retire(ctx, "r", GenA); !errors.Is(err, ErrSessionsNotEnded) {
		t.Errorf("insufficient privilege: %v", err)
	}
	f.want(t, `|ALTER ROLE "r_a" WITH NOLOGIN PASSWORD NULL`)
	f = &fakePG{rows: roleRows("r", true), failOn: "pg_terminate_backend", failErr: &pgconn.PgError{Code: "53300"}}
	if err := f.admin().Retire(ctx, "r", GenA); err == nil || errors.Is(err, ErrSessionsNotEnded) {
		t.Errorf("other error: %v", err)
	}
}

// The marker is written after the grants, and a role that is not ours is
// refused, whichever of the three it is.
func TestApplyRoleOrderAndOwnership(t *testing.T) {
	ctx := context.Background()
	p := RoleParams{
		Base: "r", Database: "db", Marker: Marker{Claim: "team-a/app/uid-1", Privileges: ReadOnly, Active: GenA},
		ConnLimit: -1, Password: "pw", Privileges: ReadOnly,
	}
	f := &fakePG{}
	if err := f.admin().ApplyRole(ctx, p); err != nil {
		t.Fatal(err)
	}
	final := `|COMMENT ON ROLE "r" IS 'craftainer.dev/claim=team-a/app/uid-1 privileges=ReadOnly active=a'`
	grant := `db|GRANT USAGE ON SCHEMA public TO "r"`
	if last := f.stmts[len(f.stmts)-1]; last != final {
		t.Errorf("the marker is not written last: %v", f.stmts)
	}
	if strings.Index(f.joined(), grant) > strings.LastIndex(f.joined(), final) {
		t.Errorf("the marker precedes the grants:\n%s", f.joined())
	}

	for _, name := range []string{"r", "r_a", "r_b"} {
		rows := roleRows("r", true)
		for _, row := range rows[0].rows {
			if row["name"] == name {
				row["comment"] = "someone/else/uid"
				if name != "r" {
					row["comment"] = ""
				}
			}
		}
		f = &fakePG{rows: rows}
		if err := f.admin().ApplyRole(ctx, p); !errors.Is(err, ErrNotOwned) {
			t.Errorf("%s: %v", name, err)
		}
		f.wantNot(t, "CREATE", "ALTER", "GRANT", "COMMENT")
	}
}

// A connection failure reaches a condition and an Event as one of a few fixed
// texts, and never as the raw error.
func TestConnectionErrorsAreCoarse(t *testing.T) {
	secret := "10.1.2.3:5432 pg.internal.example"
	wrap := func(err error) error { return fmt.Errorf("dial tcp %s: %w", secret, err) }
	for name, tc := range map[string]struct {
		err      error
		category error
	}{
		"policy":        {wrap(egress.ErrBlocked), ErrConnPolicy},
		"auth":          {wrap(&pgconn.PgError{Code: "28P01", Message: secret}), ErrConnAuth},
		"verify":        {wrap(&tls.CertificateVerificationError{Err: errors.New("x509: " + secret)}), ErrConnTLS},
		"tls text":      {wrap(errors.New("tls: handshake failure")), ErrConnTLS},
		"deadline":      {wrap(context.DeadlineExceeded), ErrConnTimeout},
		"net timeout":   {wrap(&net.DNSError{Name: secret, IsTimeout: true}), ErrConnTimeout},
		"refused":       {wrap(errors.New("connection refused")), ErrConnFailed},
		"host with tls": {wrap(errors.New("lookup outls.example: no such host")), ErrConnFailed},
		"other pgerror": {wrap(&pgconn.PgError{Code: "53300", Message: secret}), ErrConnFailed},
	} {
		f := &fakePG{openErr: map[string]error{"": tc.err}}
		_, err := f.admin().Database(context.Background(), "x")
		if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, tc.category) || !errors.Is(err, tc.err) {
			t.Errorf("%s: %v", name, err)
		}
		if want := "server unreachable: " + tc.category.Error(); err == nil || err.Error() != want {
			t.Errorf("%s: message %q, want %q", name, err, want)
		}
	}
}

func TestDropRole(t *testing.T) {
	ctx := context.Background()
	f := &fakePG{rows: roleRows("r", true)}
	if err := f.admin().DropRole(ctx, "r", "team-a/app/uid-1", "db"); err != nil {
		t.Fatal(err)
	}
	f.want(t,
		`pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename IN ('r', 'r_a', 'r_b')`,
		`db|REASSIGN OWNED BY "r", "r_a", "r_b" TO CURRENT_USER`,
		`db|DROP OWNED BY "r", "r_a", "r_b"`,
		`|DROP ROLE IF EXISTS "r", "r_a", "r_b"`)

	// A role of another claim, or without a marker, is refused and nothing is
	// dropped, whichever of the three it is.
	for _, foreign := range []string{"someone/else/uid", ""} {
		f = &fakePG{rows: roleRows("r", true)}
		f.rows[0].rows[1]["comment"] = markerPrefix + foreign
		if foreign == "" {
			f.rows[0].rows[1]["comment"] = ""
		}
		err := f.admin().DropRole(ctx, "r", "team-a/app/uid-1", "db")
		if !errors.Is(err, ErrNotOwned) || strings.Contains(f.joined(), "DROP") || strings.Contains(f.joined(), "terminate") {
			t.Errorf("foreign %q: %v, %v", foreign, err, f.stmts)
		}
	}

	// Nothing left to drop.
	f = &fakePG{}
	if err := f.admin().DropRole(ctx, "r", "team-a/app/uid-1", "db"); err != nil || len(f.stmts) != 1 {
		t.Errorf("empty drop: %v, %v", err, f.stmts)
	}

	// The database is gone: its owned objects went with it.
	f = &fakePG{rows: roleRows("r", true), openErr: map[string]error{"db": &pgconn.PgError{Code: "3D000"}}}
	if err := f.admin().DropRole(ctx, "r", "team-a/app/uid-1", "db"); err != nil {
		t.Fatal(err)
	}
	f.want(t, `DROP ROLE IF EXISTS`)

	// An admin that may not end sessions still drops the roles.
	f = &fakePG{rows: roleRows("r", true), failOn: "pg_terminate_backend", failErr: &pgconn.PgError{Code: "42501"}}
	if err := f.admin().DropRole(ctx, "r", "team-a/app/uid-1", "db"); err != nil {
		t.Errorf("insufficient privilege: %v", err)
	}
	f.want(t, `|DROP ROLE IF EXISTS "r", "r_a", "r_b"`)

	for _, fail := range []string{"pg_roles", "pg_terminate_backend", "REASSIGN OWNED", "DROP ROLE"} {
		f = &fakePG{rows: roleRows("r", true), failOn: fail}
		if err := f.admin().DropRole(ctx, "r", "team-a/app/uid-1", "db"); !errors.Is(err, errBoom) {
			t.Errorf("failOn %q: %v", fail, err)
		}
	}
}

// DropOwnRoles drops the roles of the claim and skips the others, whichever of
// the three is foreign; a set with nothing of the claim drops nothing.
func TestDropOwnRoles(t *testing.T) {
	ctx := context.Background()
	const claim = "team-a/app/uid-1"
	for _, tc := range []struct {
		name    string
		foreign []int // indexes into the rows: 0 owner, 1 login a, 2 login b
		skipped []string
		list    string // the roles that are dropped, "" for none
	}{
		{"all ours", nil, nil, `"r", "r_a", "r_b"`},
		{"login b foreign", []int{2}, []string{"r_b"}, `"r", "r_a"`},
		{"login a foreign", []int{1}, []string{"r_a"}, `"r", "r_b"`},
		{"owner foreign", []int{0}, []string{"r"}, `"r_a", "r_b"`},
		{"owner and login a foreign", []int{0, 1}, []string{"r", "r_a"}, `"r_b"`},
		{"only the owner is ours", []int{1, 2}, []string{"r_a", "r_b"}, `"r"`},
		{"all foreign", []int{0, 1, 2}, []string{"r", "r_a", "r_b"}, ""},
	} {
		name := tc.name
		f := &fakePG{rows: roleRows("r", true)}
		for _, i := range tc.foreign {
			f.rows[0].rows[i]["comment"] = markerPrefix + "someone/else/uid"
		}
		if len(tc.foreign) == 1 && tc.foreign[0] == 2 {
			f.rows[0].rows[2]["comment"] = "" // no marker at all
		}
		skipped, err := f.admin().DropOwnRoles(ctx, "r", claim, "db")
		if err != nil || !slices.Equal(skipped, tc.skipped) {
			t.Errorf("%s: skipped %v, %v", name, skipped, err)
		}
		if tc.list == "" {
			f.wantNot(t, "DROP", "terminate", "REASSIGN")
			continue
		}
		if !slices.Contains(f.stmts, "|DROP ROLE IF EXISTS "+tc.list) ||
			!slices.Contains(f.stmts, "db|REASSIGN OWNED BY "+tc.list+" TO CURRENT_USER") {
			t.Errorf("%s: statements %v", name, f.stmts)
		}
	}

	// A failing drop or read is reported.
	f := &fakePG{rows: roleRows("r", true), failOn: "DROP ROLE"}
	if _, err := f.admin().DropOwnRoles(ctx, "r", claim, "db"); !errors.Is(err, errBoom) {
		t.Errorf("failing drop: %v", err)
	}
	f = &fakePG{failOn: "pg_roles"}
	if _, err := f.admin().DropOwnRoles(ctx, "r", claim, "db"); !errors.Is(err, errBoom) {
		t.Errorf("failing read: %v", err)
	}
}

// iamRoleRows is the one login role of an IAM-mode claim, as Observe reads it.
func iamRoleRows(base, comment string) []scripted {
	return []scripted{{"FROM pg_roles", []Row{
		{"name": base, "login": "true", "connlimit": "-1", "comment": comment, "rdsiam": "true"},
	}}}
}

// An IAM-mode role is one login role with rds_iam and no password, quoted as
// an identifier, with its marker written with the role and again after the
// grants.
func TestApplyIAMRole(t *testing.T) {
	ctx := context.Background()
	p := RoleParams{
		Base: `team-a_a"pp_x`, Database: "team-a_db_y", Marker: Marker{Claim: "team-a/app/uid-1", Privileges: ReadWrite},
		ConnLimit: 7, Privileges: ReadWrite,
	}
	f := &fakePG{}
	if err := f.admin().ApplyIAMRole(ctx, p); err != nil {
		t.Fatal(err)
	}
	q := `"team-a_a""pp_x"`
	f.want(t,
		`|CREATE ROLE `+q+` LOGIN; COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=team-a/app/uid-1'`,
		`|ALTER ROLE `+q+` WITH LOGIN PASSWORD NULL CONNECTION LIMIT 7`,
		`|GRANT "rds_iam" TO `+q,
		`|GRANT CONNECT ON DATABASE "team-a_db_y" TO `+q,
		`team-a_db_y|GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+q,
		`|COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=team-a/app/uid-1 privileges=ReadWrite'`)
	f.wantNot(t, "NOLOGIN", "SET role")
	if strings.Contains(strings.ReplaceAll(f.joined(), "PASSWORD NULL", ""), "PASSWORD") {
		t.Errorf("a password is set:\n%s", f.joined())
	}
	if last := f.stmts[len(f.stmts)-1]; !strings.HasPrefix(last, `|COMMENT ON ROLE `) {
		t.Errorf("the marker is not written last: %v", f.stmts)
	}

	// A role of ours is updated, not created again; Owner gets ALL.
	f = &fakePG{rows: iamRoleRows(p.Base, "craftainer.dev/claim=team-a/app/uid-1 privileges=ReadWrite")}
	p.Privileges = Owner
	if err := f.admin().ApplyIAMRole(ctx, p); err != nil {
		t.Fatal(err)
	}
	f.wantNot(t, "CREATE ROLE")
	f.want(t, `|GRANT "rds_iam" TO `+q, `GRANT ALL ON DATABASE "team-a_db_y" TO `+q)

	// A role of that name that is not ours is refused before any statement,
	// whether it has another claim's marker, a comment that is no marker or none.
	for _, comment := range []string{"craftainer.dev/claim=someone/else/uid", "hand made", ""} {
		f = &fakePG{rows: iamRoleRows(p.Base, comment)}
		if err := f.admin().ApplyIAMRole(ctx, p); !errors.Is(err, ErrNotOwned) {
			t.Errorf("comment %q: %v", comment, err)
		}
		f.wantNot(t, "CREATE", "ALTER", "GRANT", "COMMENT ON")
	}

	bad := p
	bad.Privileges = "Root"
	if err := (&fakePG{}).admin().ApplyIAMRole(ctx, bad); !errors.Is(err, ErrInvalidPrivileges) {
		t.Errorf("unknown privileges: %v", err)
	}
	if err := (&fakePG{failOn: "pg_roles"}).admin().ApplyIAMRole(ctx, p); !errors.Is(err, errBoom) {
		t.Errorf("observe failure: %v", err)
	}
	for _, fail := range []string{"CREATE ROLE", "ON SCHEMA public", "COMMENT ON ROLE"} {
		f = &fakePG{failOn: fail}
		if err := f.admin().ApplyIAMRole(ctx, p); !errors.Is(err, errBoom) {
			t.Errorf("failOn %q: %v", fail, err)
		}
	}
}

// A Cloud SQL IAM role is created by the Admin API, not by SQL: insert runs
// only for a role that does not exist, and the marker is written right after
// it, before the grants and again after them. The name is quoted.
func TestApplyCloudSQLIAMRole(t *testing.T) {
	ctx := context.Background()
	p := RoleParams{
		Base: `app@proj-12345.iam`, Database: "team-a_db_y", Marker: Marker{Claim: "team-a/app/uid-1", Privileges: ReadWrite},
		ConnLimit: 7, Privileges: ReadWrite,
	}
	q := `"app@proj-12345.iam"`
	inserts := 0
	insert := func(context.Context) error { inserts++; return nil }

	f := &fakePG{}
	if err := f.admin().ApplyCloudSQLIAMRole(ctx, p, insert); err != nil {
		t.Fatal(err)
	}
	if inserts != 1 {
		t.Errorf("insert ran %d times", inserts)
	}
	f.want(t,
		`|COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=team-a/app/uid-1'`,
		`|ALTER ROLE `+q+` WITH CONNECTION LIMIT 7`,
		`|GRANT CONNECT ON DATABASE "team-a_db_y" TO `+q,
		`team-a_db_y|GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+q,
		`|COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=team-a/app/uid-1 privileges=ReadWrite'`)
	f.wantNot(t, "CREATE ROLE", "PASSWORD", `"rds_iam"`, "NOLOGIN")
	// The first marker comes before the grants.
	first := slices.IndexFunc(f.stmts, func(s string) bool { return strings.Contains(s, "COMMENT ON ROLE") })
	grant := slices.IndexFunc(f.stmts, func(s string) bool { return strings.Contains(s, "GRANT CONNECT") })
	if first < 0 || grant < 0 || first > grant {
		t.Errorf("the marker is not written right after the insert: %v", f.stmts)
	}

	// A role of ours is updated, and not inserted again; the connection limit is
	// changed only when it differs.
	ours := "craftainer.dev/claim=team-a/app/uid-1 privileges=ReadWrite"
	f = &fakePG{rows: iamRoleRows(p.Base, ours)}
	inserts = 0
	if err := f.admin().ApplyCloudSQLIAMRole(ctx, p, insert); err != nil || inserts != 0 {
		t.Fatalf("existing role: %v, inserts %d", err, inserts)
	}
	f.want(t, `|ALTER ROLE `+q+` WITH CONNECTION LIMIT 7`)
	p.ConnLimit = -1
	f = &fakePG{rows: iamRoleRows(p.Base, ours)}
	if err := f.admin().ApplyCloudSQLIAMRole(ctx, p, insert); err != nil {
		t.Fatal(err)
	}
	f.wantNot(t, "CONNECTION LIMIT", "CREATE ROLE")

	// A role of that name that is not ours is refused before the insert.
	for _, comment := range []string{"craftainer.dev/claim=someone/else/uid", "hand made", ""} {
		f = &fakePG{rows: iamRoleRows(p.Base, comment)}
		inserts = 0
		if err := f.admin().ApplyCloudSQLIAMRole(ctx, p, insert); !errors.Is(err, ErrNotOwned) || inserts != 0 {
			t.Errorf("comment %q: %v, inserts %d", comment, err, inserts)
		}
		f.wantNot(t, "ALTER", "GRANT", "COMMENT ON")
	}

	bad := p
	bad.Privileges = "Root"
	if err := (&fakePG{}).admin().ApplyCloudSQLIAMRole(ctx, bad, insert); !errors.Is(err, ErrInvalidPrivileges) {
		t.Errorf("unknown privileges: %v", err)
	}
	if err := (&fakePG{failOn: "pg_roles"}).admin().ApplyCloudSQLIAMRole(ctx, p, insert); !errors.Is(err, errBoom) {
		t.Errorf("observe failure: %v", err)
	}
	// A failed insert writes nothing.
	f = &fakePG{}
	if err := f.admin().ApplyCloudSQLIAMRole(ctx, p, func(context.Context) error { return errBoom }); !errors.Is(err, errBoom) {
		t.Errorf("insert failure: %v", err)
	}
	f.wantNot(t, "COMMENT ON", "GRANT")
	for _, fail := range []string{"COMMENT ON ROLE", "ON SCHEMA public"} {
		f = &fakePG{failOn: fail}
		if err := f.admin().ApplyCloudSQLIAMRole(ctx, p, insert); !errors.Is(err, errBoom) {
			t.Errorf("failOn %q: %v", fail, err)
		}
	}
}

// Dropping a Cloud SQL IAM role ends its sessions, hands its objects to the
// admin role and has the Admin API delete the user; a role that is not the
// claim's is left alone.
func TestDropCloudSQLIAMRole(t *testing.T) {
	ctx := context.Background()
	const claim = "team-a/app/uid-1"
	base := "app@proj-12345.iam"
	q := `"` + base + `"`
	ours := iamRoleRows(base, markerPrefix+claim)
	removed := 0
	remove := func(context.Context) error { removed++; return nil }

	f := &fakePG{rows: ours}
	skipped, err := f.admin().DropCloudSQLIAMRole(ctx, base, claim, "db", remove)
	if err != nil || skipped || removed != 1 {
		t.Fatalf("drop: %v, skipped %v, removed %d", err, skipped, removed)
	}
	f.want(t, `|SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = '`+base+`'`,
		`|GRANT `+q+` TO CURRENT_USER`, `db|REASSIGN OWNED BY `+q+` TO CURRENT_USER`, `db|DROP OWNED BY `+q)
	f.wantNot(t, "DROP ROLE")

	// Foreign roles are skipped, and so is a role that is not there.
	removed = 0
	for _, comment := range []string{markerPrefix + "someone/else/uid", "hand made", ""} {
		f = &fakePG{rows: iamRoleRows(base, comment)}
		skipped, err = f.admin().DropCloudSQLIAMRole(ctx, base, claim, "db", remove)
		if err != nil || !skipped || removed != 0 || strings.Contains(f.joined(), "REASSIGN") {
			t.Errorf("comment %q: %v, skipped %v, removed %d", comment, err, skipped, removed)
		}
	}
	skipped, err = (&fakePG{}).admin().DropCloudSQLIAMRole(ctx, base, claim, "db", remove)
	if err != nil || skipped || removed != 0 {
		t.Errorf("absent role: %v, skipped %v, removed %d", err, skipped, removed)
	}

	// The database is gone: its objects went with it. An admin that may not end
	// sessions, or may not grant the membership, still goes on.
	f = &fakePG{rows: ours, openErr: map[string]error{"db": &pgconn.PgError{Code: "3D000"}}}
	if _, err = f.admin().DropCloudSQLIAMRole(ctx, base, claim, "db", remove); err != nil || removed != 1 {
		t.Errorf("database gone: %v, removed %d", err, removed)
	}
	f = &fakePG{rows: ours, failOn: "pg_terminate_backend", failErr: &pgconn.PgError{Code: "42501"}}
	if _, err = f.admin().DropCloudSQLIAMRole(ctx, base, claim, "db", remove); err != nil || removed != 2 {
		t.Errorf("no pg_signal_backend: %v, removed %d", err, removed)
	}
	f = &fakePG{rows: ours, failOn: "GRANT " + q}
	if _, err = f.admin().DropCloudSQLIAMRole(ctx, base, claim, "db", remove); err != nil || removed != 3 {
		t.Errorf("grant refused: %v, removed %d", err, removed)
	}

	for _, fail := range []string{"pg_roles", "pg_terminate_backend", "REASSIGN OWNED"} {
		f = &fakePG{rows: ours, failOn: fail}
		if _, err = f.admin().DropCloudSQLIAMRole(ctx, base, claim, "db", remove); !errors.Is(err, errBoom) {
			t.Errorf("failOn %q: %v", fail, err)
		}
	}
	// The user is deleted last, and its failure is the error.
	if _, err = (&fakePG{rows: ours}).admin().DropCloudSQLIAMRole(ctx, base, claim, "db",
		func(context.Context) error { return errBoom }); !errors.Is(err, errBoom) {
		t.Errorf("remove failure: %v", err)
	}
}
