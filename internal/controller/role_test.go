package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// roleAnswer answers the role query as roles that exist.
func roleAnswer(base, claim, privileges, active string, aLogin, bLogin bool) func(string) []pgclient.Row {
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	return func(sql string) []pgclient.Row {
		if !strings.Contains(sql, "FROM pg_roles") {
			return nil
		}
		return []pgclient.Row{
			{"name": base, "login": "false", "connlimit": "-1",
				"comment": pgclient.Marker{Claim: claim, Privileges: privileges, Active: active}.String()},
			{"name": base + "_a", "login": b(aLogin), "connlimit": "-1", "comment": pgclient.Marker{Claim: claim}.String()},
			{"name": base + "_b", "login": b(bLogin), "connlimit": "-1", "comment": pgclient.Marker{Claim: claim}.String()},
		}
	}
}

func bindingSecret(h *harness, role *dbv1alpha1.DatabaseRole) map[string]string {
	h.t.Helper()
	var s corev1.Secret
	h.must(env.Client.Get(context.Background(), client.ObjectKey{Namespace: h.ns, Name: role.GetBindingSecretName()}, &s))
	if s.Type != "servicebinding.io/postgresql" {
		h.t.Errorf("Secret type = %q", s.Type)
	}
	out := map[string]string{}
	for k, v := range s.Data {
		out[k] = string(v)
	}
	return out
}

func TestRoleLifecycleAndRotation(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	dbr := &DatabaseReconciler{h.cfg}
	rr := &RoleReconciler{h.cfg}
	db := h.database("orders", nil)
	h.reconcileOK(dbr, db)
	dbName := pgclient.DatabaseName(h.ns, "orders")

	grace := metav1.Duration{Duration: time.Hour}
	interval := metav1.Duration{Duration: 24 * time.Hour}
	role := h.role("orders-app", "orders", func(r *dbv1alpha1.DatabaseRole) {
		r.Spec.Privileges = dbv1alpha1.PrivilegesReadOnly
		r.Spec.RotationPolicy = &common.RotationPolicy{Interval: &interval, GracePeriod: &grace}
	})
	base := pgclient.RoleBase(h.ns, "orders-app")

	// Create: the owner role, both logins, privileges on the database, and the
	// binding Secret with the first login (a).
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.get(role)
	claim := h.ns + "/orders-app/" + string(role.UID)
	if role.Status.ExternalName != base || role.Status.Binding == nil || role.Status.Binding.Name != "orders-app" {
		t.Errorf("status = %+v", role.Status)
	}
	h.fake.want(t,
		`|CREATE ROLE "`+base+`" NOLOGIN`,
		`|ALTER ROLE "`+base+`_a" WITH LOGIN PASSWORD '`,
		`|ALTER ROLE "`+base+`_b" WITH CONNECTION LIMIT -1`,
		`|COMMENT ON ROLE "`+base+`" IS 'craftainer.dev/claim=`+claim+` privileges=ReadOnly active=a'`,
		`|GRANT CONNECT ON DATABASE "`+dbName+`" TO "`+base+`"`,
		dbName+`|GRANT SELECT ON ALL TABLES IN SCHEMA public TO "`+base+`"`)
	data := bindingSecret(h, role)
	if data["username"] != base+"_a" || data["database"] != dbName || data["host"] != "pg.example.test" ||
		data["port"] != "5432" || data["sslmode"] != "verify-full" || data["type"] != "postgresql" ||
		data["provider"] != "craftainer.dev" || len(data["password"]) < 40 {
		t.Errorf("binding = %v", data)
	}
	if !strings.HasPrefix(data["uri"], "postgresql://"+base+"_a:"+data["password"]+"@pg.example.test:5432/"+dbName+"?sslmode=verify-full") {
		t.Errorf("uri = %q", data["uri"])
	}
	// The password is in the Secret only: not in an Event, a condition or a log
	// of statements the operator keeps.
	h.rec.AssertNoSecret(t, data["password"])
	for _, c := range role.Status.Conditions {
		if strings.Contains(c.Message, data["password"]) {
			t.Errorf("the password is in condition %s", c.Type)
		}
	}
	h.fake.wantNot(t, "PASSWORD NULL")
	firstPassword := data["password"]

	// Steady state: nothing is written and the Secret is kept.
	h.fake.answer = roleAnswer(base, claim, "ReadOnly", "a", true, false)
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.wantNot(t, "CREATE", "ALTER", "COMMENT", "GRANT", "DROP")
	if bindingSecret(h, role)["password"] != firstPassword {
		t.Error("the password changed without a reason")
	}

	// Rotation: the annotation issues login b; a stays valid for the grace period.
	h.get(role)
	patch := client.MergeFrom(role.DeepCopy())
	role.Annotations = map[string]string{lifecycle.AnnotationRotate: "1"}
	h.must(env.Client.Patch(context.Background(), role, patch))
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `|ALTER ROLE "`+base+`_b" WITH LOGIN PASSWORD '`, `active=b'`)
	h.fake.wantNot(t, "PASSWORD NULL")
	data = bindingSecret(h, role)
	if data["username"] != base+"_b" || data["password"] == firstPassword {
		t.Errorf("rotated binding = %v", data)
	}
	h.get(role)
	if role.Status.Rotation == nil || role.Status.Rotation.Active != "b" {
		t.Fatalf("rotation = %+v", role.Status.Rotation)
	}

	// After the grace period the previous login is retired.
	h.fake.answer = roleAnswer(base, claim, "ReadOnly", "b", true, true)
	h.now = h.now.Add(2 * time.Hour)
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `|ALTER ROLE "`+base+`_a" WITH NOLOGIN PASSWORD NULL`)
	h.fake.wantNot(t, `"`+base+`_b" WITH NOLOGIN`)
	h.rec.AssertEvent(t, "Normal", conditions.EventRotationFinished, "")

	// Drift: the privileges changed in the spec; the role is updated and the
	// active login (b, from the marker) gets a new password.
	h.get(role)
	patch = client.MergeFrom(role.DeepCopy())
	role.Spec.Privileges = dbv1alpha1.PrivilegesOwner
	h.must(env.Client.Patch(context.Background(), role, patch))
	h.fake.answer = roleAnswer(base, claim, "ReadOnly", "b", false, true)
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `|ALTER ROLE "`+base+`_b" WITH LOGIN PASSWORD '`, `privileges=Owner active=b'`,
		`|GRANT ALL ON DATABASE "`+dbName+`" TO "`+base+`"`)
	if bindingSecret(h, role)["username"] != base+"_b" {
		t.Error("the Secret does not hold the active login")
	}

	// A role someone else owns is never touched.
	h.fake.answer = roleAnswer(base, "other/role/uid", "Owner", "a", true, false)
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.get(role)
	h.wantCondition(role.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	h.fake.wantNot(t, "CREATE", "ALTER", "GRANT", "DROP")

	// Deleting drops every generation, the sessions and the owned objects.
	h.fake.answer = roleAnswer(base, claim, "Owner", "b", false, true)
	h.must(env.Client.Delete(context.Background(), role))
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `pg_terminate_backend(pid)`,
		dbName+`|REASSIGN OWNED BY "`+base+`", "`+base+`_a", "`+base+`_b" TO CURRENT_USER`,
		`|DROP ROLE IF EXISTS "`+base+`", "`+base+`_a", "`+base+`_b"`)
	if !h.gone(role) {
		t.Error("the claim is still there")
	}
}

func TestRoleNeedsAUsableDatabase(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	rr := &RoleReconciler{h.cfg}

	// No such Database: nothing is created.
	role := h.role("no-db", "absent", nil)
	if err := h.reconcile(rr, role); !errors.Is(err, ErrDatabaseNotUsable) {
		t.Errorf("missing database: %v", err)
	}
	h.fake.wantNot(t, "CREATE")

	// A Database on another server is not usable either.
	other := h.database("elsewhere", func(d *dbv1alpha1.Database) { d.Spec.ServerRef.Name = "another-server" })
	role2 := h.role("wrong-server", other.Name, nil)
	if err := h.reconcile(rr, role2); !errors.Is(err, ErrDatabaseNotUsable) || !strings.Contains(err.Error(), "another-server") {
		t.Errorf("database on another server: %v", err)
	}

	// Deleting a role does not need the Database (it may be gone first).
	h.must(env.Client.Delete(context.Background(), role))
	h.reconcileOK(rr, role)
	if !h.gone(role) {
		t.Error("the claim is still there")
	}

	// A failing read of the Database is an error.
	boom := errors.New("api server down")
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(err)
	cfg := h.cfg
	cfg.Client = interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*dbv1alpha1.Database); ok {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := h.reconcile(&RoleReconciler{cfg}, role2); !errors.Is(err, boom) {
		t.Errorf("database read: %v", err)
	}
}

// Without Update in managementPolicies the role is not rotated, and a claim
// that does not exist is no error.
func TestRoleReconcileOfAMissingClaim(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	missing := &dbv1alpha1.DatabaseRole{ObjectMeta: metav1.ObjectMeta{Name: "never", Namespace: h.ns}}
	h.reconcileOK(&RoleReconciler{h.cfg}, missing)
}

func TestRoleDataWithoutPortOrSSLMode(t *testing.T) {
	be := &backend{}
	be.spec.Host = "pg.example.test"
	be.conn.CABundle = []byte("PEM")
	e := &roleExternal{be: be, base: "b", database: "d"}
	d := e.data("a", "pw")
	if string(d["port"]) != "5432" || string(d["sslmode"]) != "verify-full" || string(d["sslrootcert"]) != "PEM" ||
		string(d["username"]) != "b_a" {
		t.Errorf("data = %v", d)
	}
}

// A failing statement during create or rotation is reported and retried.
func TestRoleStatementFailures(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	role := h.role("flaky", "orders", nil)
	base := pgclient.RoleBase(h.ns, "flaky")

	h.fake.failOn = "CREATE ROLE"
	if err := h.reconcile(rr, role); !errors.Is(err, errStatement) {
		t.Errorf("create: %v", err)
	}
	h.fake.failOn = ""
	h.reconcileOK(rr, role)
	h.get(role)
	claim := h.ns + "/flaky/" + string(role.UID)

	// A rotation whose statement fails leaves the active login and the Secret
	// alone.
	h.fake.answer = roleAnswer(base, claim, "ReadWrite", "a", true, false)
	before := bindingSecret(h, role)
	patch := client.MergeFrom(role.DeepCopy())
	role.Annotations = map[string]string{lifecycle.AnnotationRotate: "1"}
	h.must(env.Client.Patch(context.Background(), role, patch))
	h.fake.failOn = "WITH LOGIN PASSWORD"
	if err := h.reconcile(rr, role); !errors.Is(err, errStatement) {
		t.Errorf("rotate: %v", err)
	}
	if bindingSecret(h, role)["password"] != before["password"] {
		t.Error("the Secret changed although the rotation failed")
	}
	h.rec.AssertEvent(t, "Warning", conditions.EventRotationFailed, "")
}
