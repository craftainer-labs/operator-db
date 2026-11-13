package pgclient

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

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
		{"name": base + "_a", "login": l, "connlimit": "2"},
		{"name": base + "_b", "login": "false", "connlimit": "-1"},
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
	if st.Login(GenA) != st.A || st.Login(GenB) != st.B {
		t.Error("Login picks the wrong role")
	}
	f.want(t, `rolname IN ('r', 'r_a', 'r_b')`)

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
		`|ALTER ROLE "team-a_app_x_a" WITH LOGIN PASSWORD 'p''w' CONNECTION LIMIT 7`,
		`|ALTER ROLE "team-a_app_x_b" WITH CONNECTION LIMIT 7`,
		`|GRANT "team-a_app_x" TO "team-a_app_x_a"`,
		`|ALTER ROLE "team-a_app_x_b" SET role = 'team-a_app_x'`,
		`|COMMENT ON ROLE "team-a_app_x" IS 'craftainer.dev/claim=team-a/app/uid-1 privileges=ReadWrite active=a'`,
		`|GRANT CONNECT ON DATABASE "team-a_db_y" TO "team-a_app_x"`,
		`team-a_db_y|GRANT USAGE ON SCHEMA public TO "team-a_app_x"`,
		`team-a_db_y|GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO "team-a_app_x"`)
	f.wantNot(t, "GRANT ALL ON DATABASE")

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
	f = &fakePG{failOn: "CREATE ROLE"}
	if err := f.admin().ApplyRole(ctx, p); !errors.Is(err, errBoom) {
		t.Errorf("role failure: %v", err)
	}
	f = &fakePG{failOn: "ON SCHEMA public"}
	if err := f.admin().ApplyRole(ctx, p); !errors.Is(err, errBoom) {
		t.Errorf("grant failure: %v", err)
	}
}

func TestActivateAndRetire(t *testing.T) {
	ctx := context.Background()
	f := &fakePG{}
	if err := f.admin().Activate(ctx, "r", GenB, "pw", Marker{Claim: "c", Privileges: ReadOnly, Active: GenA}); err != nil {
		t.Fatal(err)
	}
	f.want(t, `|ALTER ROLE "r_b" WITH LOGIN PASSWORD 'pw'`, `COMMENT ON ROLE "r" IS 'craftainer.dev/claim=c privileges=ReadOnly active=b'`)

	f = &fakePG{rows: roleRows("r", true)}
	if err := f.admin().Retire(ctx, "r", GenA); err != nil {
		t.Fatal(err)
	}
	f.want(t, `|ALTER ROLE "r_a" WITH NOLOGIN PASSWORD NULL`)

	// A login that is already gone is fine.
	f = &fakePG{}
	if err := f.admin().Retire(ctx, "r", GenA); err != nil {
		t.Fatal(err)
	}
	f.wantNot(t, "ALTER ROLE")
	f = &fakePG{failOn: "pg_roles"}
	if err := f.admin().Retire(ctx, "r", GenA); !errors.Is(err, errBoom) {
		t.Errorf("observe failure: %v", err)
	}
}

func TestDropRole(t *testing.T) {
	ctx := context.Background()
	f := &fakePG{rows: roleRows("r", true)}
	if err := f.admin().DropRole(ctx, "r", "db"); err != nil {
		t.Fatal(err)
	}
	f.want(t,
		`pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename IN ('r', 'r_a', 'r_b')`,
		`db|REASSIGN OWNED BY "r", "r_a", "r_b" TO CURRENT_USER`,
		`db|DROP OWNED BY "r", "r_a", "r_b"`,
		`|DROP ROLE IF EXISTS "r", "r_a", "r_b"`)

	// Nothing left to drop.
	f = &fakePG{}
	if err := f.admin().DropRole(ctx, "r", "db"); err != nil || len(f.stmts) != 1 {
		t.Errorf("empty drop: %v, %v", err, f.stmts)
	}

	// The database is gone: its owned objects went with it.
	f = &fakePG{rows: roleRows("r", true), openErr: map[string]error{"db": &pgconn.PgError{Code: "3D000"}}}
	if err := f.admin().DropRole(ctx, "r", "db"); err != nil {
		t.Fatal(err)
	}
	f.want(t, `DROP ROLE IF EXISTS`)

	for _, fail := range []string{"pg_roles", "pg_terminate_backend", "REASSIGN OWNED", "DROP ROLE"} {
		f = &fakePG{rows: roleRows("r", true), failOn: fail}
		if err := f.admin().DropRole(ctx, "r", "db"); !errors.Is(err, errBoom) {
			t.Errorf("failOn %q: %v", fail, err)
		}
	}
}
