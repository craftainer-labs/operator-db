//go:build integration

// Package integration runs the operator's PostgreSQL client against a real
// server with TLS on (see README.md here): the same pgclient.Admin the
// controllers use, so what the fake in the unit tests only claims is checked
// against the engine.
package integration

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// ownerClaim is the claim of the Owner role in TestRoleOwnershipAndDrop.
const ownerClaim = "it/owner/uid"

// server is the PostgreSQL under test, from the environment.
type server struct {
	host     string
	port     uint16
	user     string
	password string
	ca       []byte
}

func env(t *testing.T, key, fallback string) string {
	t.Helper()
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// readFile reads a file named by the test environment through an os.Root on
// its directory, so the read cannot leave that directory.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	b, err := dir.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// load reads the connection settings. A missing one fails the test: running
// the tag without a server is a mistake, not a reason to skip.
func load(t *testing.T) server {
	t.Helper()
	port, err := strconv.ParseUint(env(t, "PG_PORT", "5432"), 10, 16)
	if err != nil {
		t.Fatalf("PG_PORT: %v", err)
	}
	caFile := os.Getenv("PG_CA_FILE")
	if caFile == "" {
		t.Fatal("PG_CA_FILE is not set; see test/integration/README.md")
	}
	ca := readFile(t, caFile)
	password := os.Getenv("PG_ADMIN_PASSWORD")
	if file := os.Getenv("PG_ADMIN_PASSWORD_FILE"); password == "" && file != "" {
		password = strings.TrimSpace(string(readFile(t, file)))
	}
	if password == "" {
		t.Fatal("PG_ADMIN_PASSWORD or PG_ADMIN_PASSWORD_FILE is not set; see test/integration/README.md")
	}
	return server{
		host: env(t, "PG_HOST", "localhost"), port: uint16(port), user: env(t, "PG_ADMIN_USER", "postgres"),
		password: password, ca: ca,
	}
}

// target is a verify-full connection to database as user.
func (s server) target(user, password, database string) pgclient.Target {
	return pgclient.Target{
		Host: s.host, Port: s.port, Database: database, User: user, Password: password, CABundle: s.ca,
		MinTLS: tls.VersionTLS12,
		// Direct dial: the egress policy refuses loopback and is tested on
		// its own; this lane tests the SQL and the TLS handshake.
		Dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
}

// admin is the Admin the controllers build, over a fresh connection per call.
func (s server) admin(mutate ...func(*pgclient.Target)) pgclient.Admin {
	return pgclient.Admin{Open: func(ctx context.Context, database string) (pgclient.Session, error) {
		if database == "" {
			database = "postgres"
		}
		t := s.target(s.user, s.password, database)
		for _, m := range mutate {
			m(&t)
		}
		return pgclient.Connect(ctx, t)
	}}
}

// as opens a session as a login role.
func (s server) as(t *testing.T, user, password, database string) (pgclient.Session, error) {
	t.Helper()
	return pgclient.Connect(context.Background(), s.target(user, password, database))
}

func mustSession(t *testing.T, s pgclient.Session, err error) pgclient.Session {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

// adminSession opens an admin session on database ("" is the admin database).
func adminSession(t *testing.T, admin pgclient.Admin, database string) pgclient.Session {
	t.Helper()
	s, err := admin.Open(context.Background(), database)
	return mustSession(t, s, err)
}

// loginSession opens a session as a login role.
func loginSession(t *testing.T, sv server, user, password, database string) pgclient.Session {
	t.Helper()
	s, err := sv.as(t, user, password, database)
	return mustSession(t, s, err)
}

func suffix() string { return strings.ToLower(rand.Text())[:8] }

// sqlState is the SQLSTATE of err, "" if it carries none.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func one(t *testing.T, s pgclient.Session, sql, column string) string {
	t.Helper()
	rows, err := s.Query(context.Background(), sql)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%s: %v, %d rows", sql, err, len(rows))
	}
	return rows[0][column]
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// eventually polls f for a few seconds: pg_terminate_backend only signals.
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestDatabaseLifecycle(t *testing.T) {
	ctx := context.Background()
	sv := load(t)
	admin := sv.admin()
	name := pgclient.DatabaseName("it-"+suffix(), "orders")
	t.Cleanup(func() { _ = admin.DropDatabase(ctx, name) })

	st, err := admin.Database(ctx, name)
	if err != nil || st.Exists {
		t.Fatalf("a database that was never created: %+v, %v", st, err)
	}

	// Create stamps the ownership marker and creates the extensions, one of
	// which has a hyphen and so only works quoted.
	marker := pgclient.Marker{Claim: "it/orders/uid-1"}
	err = admin.CreateDatabase(ctx, pgclient.DatabaseParams{
		Name: name, Marker: marker, ConnLimit: -1, Extensions: []string{"pgcrypto", "uuid-ossp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err = admin.Database(ctx, name)
	if err != nil || !st.Exists || st.Marker.Claim != marker.Claim || st.ConnLimit != -1 ||
		!contains(st.Extensions, "pgcrypto") || !contains(st.Extensions, "uuid-ossp") {
		t.Fatalf("after create: %+v, %v", st, err)
	}

	// Update changes the connection limit and adds an extension; an extension
	// that is no longer asked for stays (it could hold data).
	err = admin.UpdateDatabase(ctx, pgclient.DatabaseParams{Name: name, Marker: marker, ConnLimit: 5, Extensions: []string{"citext"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err = admin.Database(ctx, name)
	if err != nil || st.ConnLimit != 5 || !contains(st.Extensions, "citext") || !contains(st.Extensions, "pgcrypto") {
		t.Fatalf("after update: %+v, %v", st, err)
	}

	// An extension name that is not an identifier never reaches the server.
	err = admin.CreateDatabase(ctx, pgclient.DatabaseParams{Name: name + "x", Marker: marker, ConnLimit: -1, Extensions: []string{`x"; DROP`}})
	if !errors.Is(err, pgclient.ErrInvalidExtension) {
		t.Errorf("injected extension name: %v", err)
	}
	if st, err := admin.Database(ctx, name+"x"); err != nil || st.Exists {
		t.Errorf("a database was created for an invalid extension: %+v, %v", st, err)
	}

	// An extension the server does not have fails after the database is
	// created and marked, so a retry recognises the database as ours.
	missing := name + "m"
	t.Cleanup(func() { _ = admin.DropDatabase(ctx, missing) })
	err = admin.CreateDatabase(ctx, pgclient.DatabaseParams{Name: missing, Marker: marker, ConnLimit: -1, Extensions: []string{"no_such_extension"}})
	if err == nil {
		t.Error("an unavailable extension was created")
	}
	if st, err := admin.Database(ctx, missing); err != nil || !st.Exists || st.Marker.Claim != marker.Claim {
		t.Errorf("after a failed extension: %+v, %v", st, err)
	}
}

func TestDatabaseAdoptionAndDrop(t *testing.T) {
	ctx := context.Background()
	sv := load(t)
	admin := sv.admin()
	name := pgclient.DatabaseName("it-"+suffix(), "adopt")
	t.Cleanup(func() { _ = admin.DropDatabase(ctx, name) })

	// A database someone else made has no marker: Database reports it so, and
	// the controller refuses it. Only Update (after the claim recorded its
	// intent to create) stamps it.
	raw, err := admin.Open(ctx, "")
	raw = mustSession(t, raw, err)
	if err := raw.Exec(ctx, "CREATE DATABASE "+pgclient.Ident(name)); err != nil {
		t.Fatal(err)
	}
	st, err := admin.Database(ctx, name)
	if err != nil || !st.Exists || st.Marker.Claim != "" {
		t.Fatalf("unmarked database: %+v, %v", st, err)
	}
	marker := pgclient.Marker{Claim: "it/adopt/uid-2"}
	if err := admin.UpdateDatabase(ctx, pgclient.DatabaseParams{Name: name, Marker: marker, ConnLimit: -1}); err != nil {
		t.Fatal(err)
	}
	if st, err = admin.Database(ctx, name); err != nil || st.Marker.Claim != marker.Claim {
		t.Fatalf("adopted database: %+v, %v", st, err)
	}

	// A marker of another claim reads back as that claim.
	other := pgclient.Marker{Claim: "someone/else/uid-3"}
	if err := admin.UpdateDatabase(ctx, pgclient.DatabaseParams{Name: name, Marker: other, ConnLimit: -1}); err != nil {
		t.Fatal(err)
	}
	if st, err = admin.Database(ctx, name); err != nil || st.Marker.Claim != other.Claim {
		t.Fatalf("foreign marker: %+v, %v", st, err)
	}

	// Drop ends the sessions open on the database (WITH FORCE) and is
	// idempotent.
	held, err := admin.Open(ctx, name)
	held = mustSession(t, held, err)
	if err := admin.DropDatabase(ctx, name); err != nil {
		t.Fatal(err)
	}
	if st, err = admin.Database(ctx, name); err != nil || st.Exists {
		t.Fatalf("after drop: %+v, %v", st, err)
	}
	if _, err := held.Query(ctx, "SELECT 1"); err == nil {
		t.Error("a session on the dropped database survived")
	}
	if err := admin.DropDatabase(ctx, name); err != nil {
		t.Errorf("second drop: %v", err)
	}
	if s, err := admin.Open(ctx, name); err == nil {
		s.Close(ctx)
		t.Error("connected to a dropped database")
	}
}

// newDatabase creates a database for a role test and drops it afterwards.
func newDatabase(t *testing.T, admin pgclient.Admin, claim string) string {
	t.Helper()
	ctx := context.Background()
	name := pgclient.DatabaseName("it-"+suffix(), "app")
	err := admin.CreateDatabase(ctx, pgclient.DatabaseParams{Name: name, Marker: pgclient.Marker{Claim: claim}, ConnLimit: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.DropDatabase(ctx, name) })
	return name
}

func TestRoleLifecycle(t *testing.T) {
	ctx := context.Background()
	sv := load(t)
	admin := sv.admin()
	db := newDatabase(t, admin, "it/app/db-uid")
	base := pgclient.RoleBase("it-"+suffix(), "app")
	a, b := pgclient.LoginName(base, pgclient.GenA), pgclient.LoginName(base, pgclient.GenB)
	t.Cleanup(func() { _ = admin.DropRole(ctx, base, "it/app/role-uid", db) })
	claim := "it/app/role-uid"
	params := func(priv, active, password string) pgclient.RoleParams {
		return pgclient.RoleParams{
			Base: base, Database: db, ConnLimit: -1, Password: password, Privileges: priv,
			Marker: pgclient.Marker{Claim: claim, Privileges: priv, Active: active},
		}
	}

	// Create: the owner role cannot log in, both logins exist, the marker
	// names the claim.
	pw1 := rand.Text() + rand.Text()
	if err := admin.ApplyRole(ctx, params(pgclient.ReadWrite, pgclient.GenA, pw1)); err != nil {
		t.Fatal(err)
	}
	st, err := admin.Role(ctx, base)
	if err != nil || !st.Base.Exists || st.Base.Login || !st.A.Exists || !st.A.Login || !st.B.Exists || st.B.Login ||
		st.Marker.Claim != claim || st.Marker.Privileges != pgclient.ReadWrite || st.Marker.Active != pgclient.GenA ||
		st.A.Marker.Claim != claim || st.B.Marker.Claim != claim {
		t.Fatalf("after create: %+v, %v", st, err)
	}

	// The password reached the server only as a SCRAM-SHA-256 verifier.
	root := adminSession(t, admin, "")
	stored := one(t, root, "SELECT rolpassword FROM pg_authid WHERE rolname = "+pgclient.Literal(a), "rolpassword")
	if !strings.HasPrefix(stored, "SCRAM-SHA-256$4096:") || strings.Contains(stored, pw1) {
		t.Fatalf("stored password = %q", stored)
	}

	// The login authenticates with SCRAM over verified TLS, and its role is
	// the owner role.
	sess := loginSession(t, sv, a, pw1, db)
	if got := one(t, sess, "SELECT current_user AS u, session_user AS s", "u"); got != base {
		t.Errorf("current_user = %q, want %q", got, base)
	}
	if _, err := sv.as(t, a, "not-the-password", db); sqlState(err) != "28P01" {
		t.Errorf("a wrong password: %v", err)
	}

	// ReadWrite: tables the admin creates afterwards are covered by the
	// default privileges.
	table := adminSession(t, admin, db)
	if err := table.Exec(ctx, "CREATE TABLE public.items (id int)"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Exec(ctx, "INSERT INTO public.items VALUES (1)"); err != nil {
		t.Errorf("ReadWrite insert: %v", err)
	}
	if err := sess.Exec(ctx, "CREATE TABLE public.mine (id int)"); sqlState(err) != "42501" {
		t.Errorf("ReadWrite create table: %v", err)
	}

	// Update to ReadOnly with a new password: the old one stops working.
	pw2 := rand.Text() + rand.Text()
	if err := admin.ApplyRole(ctx, params(pgclient.ReadOnly, pgclient.GenA, pw2)); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.as(t, a, pw1, db); sqlState(err) != "28P01" {
		t.Errorf("the replaced password still works: %v", err)
	}
	ro := loginSession(t, sv, a, pw2, db)
	if got := one(t, ro, "SELECT count(*)::text AS n FROM public.items", "n"); got != "1" {
		t.Errorf("ReadOnly select = %q", got)
	}
	if err := ro.Exec(ctx, "INSERT INTO public.items VALUES (2)"); sqlState(err) != "42501" {
		t.Errorf("ReadOnly insert: %v", err)
	}

	// Rotation: login b becomes active while a keeps working during the grace
	// period; Retire then ends a, its password and its open sessions.
	pwB := rand.Text() + rand.Text()
	if err := admin.ApplyRole(ctx, params(pgclient.ReadOnly, pgclient.GenB, pwB)); err != nil {
		t.Fatal(err)
	}
	loginSession(t, sv, b, pwB, db)
	if _, err := sv.as(t, a, pw2, db); err != nil {
		t.Errorf("login a during the grace period: %v", err)
	}
	if err := admin.Retire(ctx, base, pgclient.GenA); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.as(t, a, pw2, db); err == nil {
		t.Error("a retired login can still connect")
	}
	if got := one(t, root, "SELECT (rolpassword IS NULL)::text AS cleared FROM pg_authid WHERE rolname = "+pgclient.Literal(a), "cleared"); got != "true" {
		t.Errorf("retired login password cleared = %q", got)
	}
	eventually(t, "the sessions of the retired login to end", func() bool {
		_, err := ro.Query(ctx, "SELECT 1")
		return err != nil
	})
	loginSession(t, sv, b, pwB, db) // the active login is untouched
	// Retiring a login that is gone is fine.
	if err := admin.Retire(ctx, base+"_gone", pgclient.GenA); err != nil {
		t.Errorf("retire of a missing role: %v", err)
	}
}

func TestRoleOwnershipAndDrop(t *testing.T) {
	ctx := context.Background()
	sv := load(t)
	admin := sv.admin()
	db := newDatabase(t, admin, "it/own/db-uid")
	root := adminSession(t, admin, "")

	// A role of another claim, or none, is refused and nothing is changed.
	foreign := pgclient.RoleBase("it-"+suffix(), "foreign")
	if err := root.Exec(ctx, "CREATE ROLE "+pgclient.Ident(foreign)+" NOLOGIN; COMMENT ON ROLE "+pgclient.Ident(foreign)+" IS 'craftainer.dev/claim=someone/else/uid'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Exec(ctx, "DROP ROLE IF EXISTS "+pgclient.Ident(foreign)) })
	err := admin.ApplyRole(ctx, pgclient.RoleParams{
		Base: foreign, Database: db, ConnLimit: -1, Password: "pw", Privileges: pgclient.ReadOnly,
		Marker: pgclient.Marker{Claim: "it/foreign/uid", Privileges: pgclient.ReadOnly, Active: pgclient.GenA},
	})
	if !errors.Is(err, pgclient.ErrNotOwned) {
		t.Errorf("a foreign role: %v", err)
	}
	if st, err := admin.Role(ctx, foreign); err != nil || st.A.Exists || st.B.Exists || st.Marker.Claim != "someone/else/uid" {
		t.Errorf("the foreign role was changed: %+v, %v", st, err)
	}
	// Drop refuses it as well, even when asked for by a claim that thinks it
	// created the role, and the role survives.
	for _, claim := range []string{"it/foreign/uid", ""} {
		if err := admin.DropRole(ctx, foreign, claim, db); !errors.Is(err, pgclient.ErrNotOwned) {
			t.Errorf("drop of a foreign role by %q: %v", claim, err)
		}
	}
	if st, err := admin.Role(ctx, foreign); err != nil || !st.Base.Exists || st.Marker.Claim != "someone/else/uid" {
		t.Errorf("the foreign role was dropped: %+v, %v", st, err)
	}

	// Owner: the login can create objects; Drop ends its sessions, hands what
	// it owns to the admin and removes all three roles.
	base := pgclient.RoleBase("it-"+suffix(), "owner")
	a := pgclient.LoginName(base, pgclient.GenA)
	pw := rand.Text() + rand.Text()
	err = admin.ApplyRole(ctx, pgclient.RoleParams{
		Base: base, Database: db, ConnLimit: -1, Password: pw, Privileges: pgclient.Owner,
		Marker: pgclient.Marker{Claim: ownerClaim, Privileges: pgclient.Owner, Active: pgclient.GenA},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.DropRole(ctx, base, ownerClaim, db) })
	sess := loginSession(t, sv, a, pw, db)
	if err := sess.Exec(ctx, "CREATE TABLE public.owned_by_role (id int)"); err != nil {
		t.Fatalf("Owner create table: %v", err)
	}
	if err := admin.DropRole(ctx, base, ownerClaim, db); err != nil {
		t.Fatal(err)
	}
	if st, err := admin.Role(ctx, base); err != nil || st.Base.Exists || st.A.Exists || st.B.Exists {
		t.Errorf("after drop: %+v, %v", st, err)
	}
	inDB := adminSession(t, admin, db)
	if got := one(t, inDB, "SELECT tableowner FROM pg_tables WHERE tablename = 'owned_by_role'", "tableowner"); got != sv.user {
		t.Errorf("the table is owned by %q, want %q", got, sv.user)
	}
	eventually(t, "the session of the dropped login to end", func() bool {
		_, err := sess.Query(ctx, "SELECT 1")
		return err != nil
	})

	// Dropping with the database already gone, and twice, succeeds.
	if err := admin.DropRole(ctx, base, ownerClaim, db+"_gone"); err != nil {
		t.Errorf("drop with the database gone: %v", err)
	}

	// A set of which one login was taken over by another claim: DropRole
	// refuses it, DropOwnRoles drops the roles that are ours and leaves that login.
	mixed := pgclient.RoleBase("it-"+suffix(), "mixed")
	mixedClaim := "it/mixed/uid"
	err = admin.ApplyRole(ctx, pgclient.RoleParams{
		Base: mixed, Database: db, ConnLimit: -1, Password: rand.Text() + rand.Text(), Privileges: pgclient.ReadOnly,
		Marker: pgclient.Marker{Claim: mixedClaim, Privileges: pgclient.ReadOnly, Active: pgclient.GenA},
	})
	if err != nil {
		t.Fatal(err)
	}
	loginB := pgclient.LoginName(mixed, pgclient.GenB)
	t.Cleanup(func() {
		_ = admin.DropRole(ctx, mixed, mixedClaim, db)
		_ = root.Exec(ctx, "DROP ROLE IF EXISTS "+pgclient.Ident(loginB))
	})
	if err := root.Exec(ctx, "COMMENT ON ROLE "+pgclient.Ident(loginB)+" IS 'craftainer.dev/claim=someone/else/uid'"); err != nil {
		t.Fatal(err)
	}
	if err := admin.DropRole(ctx, mixed, mixedClaim, db); !errors.Is(err, pgclient.ErrNotOwned) {
		t.Errorf("DropRole of a mixed set: %v", err)
	}
	skipped, err := admin.DropOwnRoles(ctx, mixed, mixedClaim, db)
	if err != nil || !slices.Equal(skipped, []string{loginB}) {
		t.Errorf("DropOwnRoles of a mixed set: skipped %v, %v", skipped, err)
	}
	if st, err := admin.Role(ctx, mixed); err != nil || st.Base.Exists || st.A.Exists || !st.B.Exists || st.Marker.Claim != "" || st.B.Marker.Claim != "someone/else/uid" {
		t.Errorf("after DropOwnRoles: %+v, %v", st, err)
	}
}

// The connection is verified and encrypted, and a failure is classified
// without leaking the address.
func TestConnectionSecurity(t *testing.T) {
	ctx := context.Background()
	sv := load(t)

	if _, err := sv.admin().Database(ctx, "postgres"); err != nil {
		t.Fatalf("the verified connection: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate   func(*pgclient.Target)
		category error
	}{
		"wrong server name": {func(t *pgclient.Target) { t.ServerName = "not-the-server.example.test" }, pgclient.ErrConnTLS},
		"unknown CA":        {func(t *pgclient.Target) { t.CABundle = nil }, pgclient.ErrConnTLS},
		"wrong password":    {func(t *pgclient.Target) { t.Password = "wrong" }, pgclient.ErrConnAuth},
	} {
		_, err := sv.admin(tc.mutate).Database(ctx, "postgres")
		if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, tc.category) {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), sv.host) {
			t.Errorf("%s: the error names the host: %v", name, err)
		}
	}

	// The server itself refuses a connection without TLS.
	plain := "host=" + sv.host + " port=" + strconv.Itoa(int(sv.port)) + " user=" + sv.user + " password=" + sv.password + " dbname=postgres sslmode=disable"
	if c, err := pgx.Connect(ctx, plain); err == nil {
		_ = c.Close(ctx)
		t.Error("the server accepted a connection without TLS")
	}
}

// The admin pool the controllers keep per server works against the engine over
// verify-full TLS: it reuses a connection, never holds more than its maximum,
// and a claim's database is opened outside it.
func TestAdminPool(t *testing.T) {
	ctx := context.Background()
	sv := load(t)
	tgt := sv.target(sv.user, sv.password, "postgres")
	pool, err := pgclient.NewPool(tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	admin := pgclient.Admin{Open: pgclient.Opener(pool, pgclient.Connect, tgt)}

	for range 5 {
		if _, err := admin.Database(ctx, "postgres"); err != nil {
			t.Fatalf("Database through the pool: %v", err)
		}
	}
	// Database("postgres") also opens postgres as a claim database, outside the
	// pool, so only the admin session was ever pooled.
	if got := pool.Stat().NewConnsCount(); got != 1 {
		t.Errorf("%d pooled connections opened for five calls, want 1", got)
	}
	if got := pool.Stat().MaxConns(); got != 2 {
		t.Errorf("MaxConns = %d, want 2", got)
	}
}

// An admin that may create roles but not terminate sessions (no
// pg_signal_backend, not a superuser, as on a managed service) still retires a
// login: it is disabled and its password cleared, the sessions stay, and the
// error says so (ErrSessionsNotEnded) instead of failing the retire.
func TestRetireWithoutTerminatePrivilege(t *testing.T) {
	ctx := context.Background()
	sv := load(t)
	root := adminSession(t, sv.admin(), "")
	limited := "it_limited_" + suffix()
	limitedPW := rand.Text() + rand.Text()
	verifier, err := pgclient.ScramVerifier(limitedPW)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Exec(ctx, "CREATE ROLE "+pgclient.Ident(limited)+" LOGIN CREATEROLE PASSWORD "+pgclient.Literal(verifier)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Exec(ctx, "DROP ROLE IF EXISTS "+pgclient.Ident(limited)) })

	// The role is created by the limited admin, which makes it that role's
	// ADMIN and so allowed to alter it.
	lim, err := sv.as(t, limited, limitedPW, "postgres")
	lim = mustSession(t, lim, err)
	base := pgclient.RoleBase("it-"+suffix(), "limited")
	login := pgclient.LoginName(base, pgclient.GenA)
	pw := rand.Text() + rand.Text()
	if err := lim.Exec(ctx, "CREATE ROLE "+pgclient.Ident(login)+" LOGIN PASSWORD "+pgclient.Literal(pw)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = lim.Exec(ctx, "DROP ROLE IF EXISTS "+pgclient.Ident(login))
		_ = root.Exec(ctx, "DROP ROLE IF EXISTS "+pgclient.Ident(login))
	})
	open := loginSession(t, sv, login, pw, "postgres")

	admin := sv.admin(func(t *pgclient.Target) { t.User, t.Password = limited, limitedPW })
	if err := admin.Retire(ctx, base, pgclient.GenA); !errors.Is(err, pgclient.ErrSessionsNotEnded) {
		t.Fatalf("Retire: %v", err)
	}
	if got := one(t, root, "SELECT rolcanlogin::text AS login FROM pg_roles WHERE rolname = "+pgclient.Literal(login), "login"); got != "false" {
		t.Errorf("rolcanlogin = %q after Retire, want false", got)
	}
	if got := one(t, root, "SELECT (rolpassword IS NULL)::text AS cleared FROM pg_authid WHERE rolname = "+pgclient.Literal(login), "cleared"); got != "true" {
		t.Errorf("password cleared = %q", got)
	}
	if _, err := open.Query(ctx, "SELECT 1"); err != nil {
		t.Errorf("the session was ended without the privilege: %v", err)
	}
	if s, err := sv.as(t, login, pw, "postgres"); err == nil {
		s.Close(ctx)
		t.Error("the retired login can open a new session")
	}
}
