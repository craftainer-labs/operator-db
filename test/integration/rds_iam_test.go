//go:build integration && rds

package integration

// The IAM mode of the AWS RDS backend (NKdb-1b): the SQL of an IAM-mode
// DatabaseRole against PostgreSQL 18, and the ARN of its binding built from
// what the probe discovers in moto. PostgreSQL is not RDS: the rds_iam role,
// which RDS provides, is created here. Nothing validates an IAM token (moto
// does not, and neither does PostgreSQL), so the login with a token is
// untested in CI (docs/nfrs/NFR-0005).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/craftainer-labs/lib-operator-go/pkg/egress"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/controller"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// rdsAdmin is the admin of sv for a server reached at a bare container address:
// the certificate names tlsServer, not the address.
func rdsAdmin(sv server) pgclient.Admin {
	return sv.admin(func(t *pgclient.Target) { t.ServerName = tlsServer })
}

// createRDSIAM creates the rds_iam role if the server does not have it, as RDS
// does for every instance.
func createRDSIAM(t *testing.T, admin pgclient.Admin) {
	t.Helper()
	s := adminSession(t, admin, "")
	err := s.Exec(context.Background(), "DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = "+
		pgclient.Literal(pgclient.RDSIAMRole)+") THEN CREATE ROLE "+pgclient.Ident(pgclient.RDSIAMRole)+"; END IF; END $$")
	if err != nil {
		t.Fatalf("create rds_iam: %v", err)
	}
}

func TestRDSIAMRole(t *testing.T) {
	ctx := context.Background()
	sv := load(t)
	admin := rdsAdmin(sv)
	createRDSIAM(t, admin)
	db := newDatabase(t, admin, "it/iam/db-uid")
	const claim = "it/iam/role-uid"
	base := pgclient.RoleBase("it-"+suffix(), "iam")
	t.Cleanup(func() { _ = admin.DropRole(ctx, base, claim, db) })
	params := func(priv string) pgclient.RoleParams {
		return pgclient.RoleParams{
			Base: base, Database: db, ConnLimit: 3, Privileges: priv,
			Marker: pgclient.Marker{Claim: claim, Privileges: priv},
		}
	}

	// Create: one login role with rds_iam, the marker, and no password.
	if err := admin.ApplyIAMRole(ctx, params(pgclient.ReadWrite)); err != nil {
		t.Fatal(err)
	}
	st, err := admin.Role(ctx, base)
	if err != nil || !st.Base.Exists || !st.Base.Login || !st.Base.RDSIAM || st.Base.ConnLimit != 3 ||
		st.Marker != (pgclient.Marker{Claim: claim, Privileges: pgclient.ReadWrite}) || st.A.Exists || st.B.Exists {
		t.Fatalf("after create: %+v, %v", st, err)
	}
	root := adminSession(t, admin, "")
	if got := one(t, root, "SELECT (rolpassword IS NULL)::text AS none FROM pg_authid WHERE rolname = "+pgclient.Literal(base), "none"); got != "true" {
		t.Errorf("the IAM role has a password: none = %q", got)
	}

	// The privileges are the role's own: as that role, ReadWrite reads and
	// writes a table the admin created, and cannot create one.
	table := adminSession(t, admin, db)
	if err := table.Exec(ctx, "CREATE TABLE public.items (id int)"); err != nil {
		t.Fatal(err)
	}
	if got := one(t, table, "SELECT has_database_privilege("+pgclient.Literal(base)+", "+pgclient.Literal(db)+", 'CONNECT')::text AS ok", "ok"); got != "true" {
		t.Errorf("CONNECT = %q", got)
	}
	as := adminSession(t, admin, db)
	if err := as.Exec(ctx, "SET ROLE "+pgclient.Ident(base)); err != nil {
		t.Fatal(err)
	}
	if err := as.Exec(ctx, "INSERT INTO public.items VALUES (1)"); err != nil {
		t.Errorf("ReadWrite insert: %v", err)
	}
	if err := as.Exec(ctx, "CREATE TABLE public.mine (id int)"); sqlState(err) != "42501" {
		t.Errorf("ReadWrite create table: %v", err)
	}

	// A password stored on the claim's role is cleared when it is applied again,
	// and it is the same role, updated, with other privileges.
	if err := root.Exec(ctx, "ALTER ROLE "+pgclient.Ident(base)+" PASSWORD 'stale'"); err != nil {
		t.Fatal(err)
	}
	if err := admin.ApplyIAMRole(ctx, params(pgclient.ReadOnly)); err != nil {
		t.Fatal(err)
	}
	if got := one(t, root, "SELECT (rolpassword IS NULL)::text AS none FROM pg_authid WHERE rolname = "+pgclient.Literal(base), "none"); got != "true" {
		t.Errorf("the stored password was not cleared: none = %q", got)
	}
	ro := adminSession(t, admin, db)
	if err := ro.Exec(ctx, "SET ROLE "+pgclient.Ident(base)); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ro, "SELECT count(*)::text AS n FROM public.items", "n"); got != "1" {
		t.Errorf("ReadOnly select = %q", got)
	}
	if err := ro.Exec(ctx, "INSERT INTO public.items VALUES (2)"); sqlState(err) != "42501" {
		t.Errorf("ReadOnly insert: %v", err)
	}
	if st, err = admin.Role(ctx, base); err != nil || st.Marker.Privileges != pgclient.ReadOnly || !st.Base.RDSIAM {
		t.Errorf("after update: %+v, %v", st, err)
	}

	// Dropping removes the one role.
	skipped, err := admin.DropOwnRoles(ctx, base, claim, db)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("drop: %v, %v", skipped, err)
	}
	if st, err = admin.Role(ctx, base); err != nil || st.Base.Exists {
		t.Errorf("after drop: %+v, %v", st, err)
	}
}

// A role of that name that the claim did not create is refused and left as it
// was, whether it has no marker or another claim's.
func TestRDSIAMRoleNameCollision(t *testing.T) {
	ctx := context.Background()
	sv := load(t)
	admin := rdsAdmin(sv)
	createRDSIAM(t, admin)
	db := newDatabase(t, admin, "it/iamx/db-uid")
	const claim = "it/iamx/role-uid"
	root := adminSession(t, admin, "")
	for name, comment := range map[string]string{"unmarked": "", "foreign": "craftainer.dev/claim=someone/else/uid"} {
		base := pgclient.RoleBase("it-"+suffix(), "iam-"+name)
		t.Cleanup(func() { _ = root.Exec(ctx, "DROP ROLE IF EXISTS "+pgclient.Ident(base)) })
		stmts := "CREATE ROLE " + pgclient.Ident(base) + " NOLOGIN"
		if comment != "" {
			stmts += "; COMMENT ON ROLE " + pgclient.Ident(base) + " IS " + pgclient.Literal(comment)
		}
		if err := root.Exec(ctx, stmts); err != nil {
			t.Fatal(err)
		}
		err := admin.ApplyIAMRole(ctx, pgclient.RoleParams{
			Base: base, Database: db, ConnLimit: -1, Privileges: pgclient.ReadWrite, Marker: pgclient.Marker{Claim: claim},
		})
		if !errors.Is(err, pgclient.ErrNotOwned) {
			t.Errorf("%s: %v", name, err)
		}
		st, rerr := admin.Role(ctx, base)
		if rerr != nil || !st.Base.Exists || st.Base.Login || st.Base.RDSIAM || st.Base.Marker.Claim == claim {
			t.Errorf("%s role was touched: %+v, %v", name, st, rerr)
		}
		if skipped, err := admin.DropOwnRoles(ctx, base, claim, db); err != nil || len(skipped) != 1 {
			t.Errorf("%s: the claim dropped a role that is not its own: %v, %v", name, skipped, err)
		}
		if st, _ = admin.Role(ctx, base); !st.Base.Exists {
			t.Errorf("%s role was dropped", name)
		}
	}
}

// The ARN of an IAM login is built from what the probe discovers in moto, and
// agrees with what moto itself reports for the instance.
func TestRDSIAMResourceARN(t *testing.T) {
	h := newRDSHarness(t, egress.ModeAllowPrivate)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := "iam-" + suffix()
	h.createInstance(t, id)

	srv := h.server(id, motoEndpoint(t))
	if _, err := (controller.Prober{Config: h.cfg}).Probe(ctx, srv, h.conn); err != nil {
		t.Fatalf("probe: %v", err)
	}
	out, err := h.rds.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(id)})
	if err != nil || len(out.DBInstances) != 1 {
		t.Fatalf("describe: %v", err)
	}
	// arn:aws:rds:<region>:<account>:db:<id>
	parts := strings.Split(aws.ToString(out.DBInstances[0].DBInstanceArn), ":")
	if len(parts) < 7 || parts[4] == "" || aws.ToString(out.DBInstances[0].DbiResourceId) == "" {
		t.Fatalf("moto's instance: %v", out.DBInstances[0])
	}
	want := "arn:aws:rds-db:" + motoRegn + ":" + parts[4] + ":dbuser:" + aws.ToString(out.DBInstances[0].DbiResourceId) + "/team_role"
	got, err := controller.IAMResourceARN(srv.Status.Discovered[dbv1alpha1.DiscoveredRegion], srv.Status.Discovered, "team_role")
	if err != nil || got != want {
		t.Errorf("ARN = %q, %v; want %q", got, err, want)
	}
}
