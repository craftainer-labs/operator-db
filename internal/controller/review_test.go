package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/egress"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// A database that exists with no marker is not adopted by its name. It is
// adopted only when the claim's status says it issued the create, and it is
// never dropped otherwise, even by a claim that is deleted with the Delete
// policy.
func TestDatabaseNeverAdoptsByNameAlone(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionDelete, nil)
	r := &DatabaseReconciler{h.cfg}
	h.fake.answer = existing("", "-1")

	taken := h.database("taken", nil)
	h.reconcileOK(r, taken)
	h.get(taken)
	msg := h.wantCondition(taken.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	if !strings.Contains(msg, ErrExistsUnmanaged.Error()) {
		t.Errorf("message = %q", msg)
	}
	h.fake.wantNot(t, "CREATE", "ALTER", "COMMENT", "DROP")

	// Deleting it leaves the database alone and releases the claim, without the
	// server being contacted: the claim never issued a create.
	h.must(env.Client.Delete(ctx, taken))
	h.fake.reset()
	h.reconcileOK(r, taken)
	if h.fake.joined() != "" {
		t.Errorf("the server was contacted:\n%s", h.fake.joined())
	}
	if !h.gone(taken) {
		t.Error("the claim was kept over a database that is not its own")
	}

	// A claim whose status records the create takes it over.
	crashed := h.database("crashed", nil)
	h.reconcileOK(r, crashed)
	h.get(crashed)
	patch := client.MergeFrom(crashed.DeepCopy())
	crashed.Status.ExternalID = createIssued
	h.must(env.Client.Status().Patch(ctx, crashed, patch))
	h.fake.reset()
	h.reconcileOK(r, crashed)
	name := pgclient.DatabaseName(h.ns, "crashed")
	h.fake.want(t, `|ALTER DATABASE "`+name+`" CONNECTION LIMIT -1`,
		`|COMMENT ON DATABASE "`+name+`" IS 'craftainer.dev/claim=`+h.ns+"/crashed/"+string(crashed.UID)+`'`)
	h.fake.wantNot(t, "CREATE DATABASE", "DROP")
}

// The create is recorded in the status before CREATE DATABASE runs, so a
// create that stops before the marker is stamped is finished by the next
// reconcile.
func TestDatabaseCreateIntentIsRecordedFirst(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("half", nil)
	h.fake.failOn = "COMMENT ON DATABASE"
	if err := h.reconcile(r, db); !errors.Is(err, errStatement) {
		t.Fatalf("reconcile: %v", err)
	}
	h.get(db)
	if db.Status.ExternalID != createIssued {
		t.Fatalf("externalId = %q", db.Status.ExternalID)
	}

	// The database exists now, without a marker.
	h.fake.failOn = ""
	h.fake.answer = existing("", "-1")
	h.fake.reset()
	h.reconcileOK(r, db)
	h.get(db)
	h.fake.want(t, `|COMMENT ON DATABASE`)
	h.fake.wantNot(t, "CREATE DATABASE")
	h.wantCondition(db.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)

	// A failing status write stops before anything is created.
	other := h.database("nostatus", nil)
	h.fake.answer = nil
	h.fake.reset()
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(err)
	cfg := h.cfg
	boom := errors.New("status write failed")
	cfg.Client = interceptor.NewClient(wc, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if d, ok := obj.(*dbv1alpha1.Database); ok && d.Status.ExternalID == createIssued {
				return boom
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	if err := h.reconcile(&DatabaseReconciler{cfg}, other); !errors.Is(err, boom) {
		t.Errorf("status write: %v", err)
	}
	h.fake.wantNot(t, "CREATE DATABASE")

	// A database made by somebody else between Observe and the create: the
	// record is withdrawn, so that deleting the claim releases it without
	// touching that database.
	raced := h.database("raced", nil)
	h.fake.answer = nil
	h.fake.failOn, h.fake.failErr = "CREATE DATABASE", &pgconn.PgError{Code: "42P04"}
	if err := h.reconcile(r, raced); err == nil {
		t.Fatal("a taken name was created")
	}
	h.get(raced)
	if raced.Status.ExternalID != "" {
		t.Errorf("externalId = %q, want it withdrawn", raced.Status.ExternalID)
	}
	h.fake.failOn, h.fake.failErr = "", nil
	h.must(env.Client.Delete(context.Background(), raced))
	h.fake.reset()
	h.reconcileOK(r, raced)
	if h.fake.joined() != "" || !h.gone(raced) {
		t.Errorf("the claim was not released without the server:\n%s", h.fake.joined())
	}
}

// Logins are marked like the owner role: an existing login without our marker
// is not ours, and nothing is created, changed or dropped.
func TestRoleNeverAdoptsAForeignLogin(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionRetain, nil)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	role := h.role("hijack", "orders", nil)
	base := pgclient.RoleBase(h.ns, "hijack")

	for _, comment := range []string{"", "craftainer.dev/claim=someone/else/uid"} {
		h.fake.answer = func(sql string) []pgclient.Row {
			if !strings.Contains(sql, "FROM pg_roles") {
				return nil
			}
			return []pgclient.Row{{"name": base + "_a", "login": "true", "connlimit": "-1", "comment": comment}}
		}
		h.fake.reset()
		h.reconcileOK(rr, role)
		h.get(role)
		h.wantCondition(role.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
		h.fake.wantNot(t, "CREATE", "ALTER", "GRANT", "COMMENT", "DROP")
	}

	// Deleting leaves the login alone and releases the claim, without the
	// server being contacted: the claim never issued a create.
	h.must(env.Client.Delete(ctx, role))
	h.fake.reset()
	h.reconcileOK(rr, role)
	if h.fake.joined() != "" {
		t.Errorf("the server was contacted:\n%s", h.fake.joined())
	}
	if !h.gone(role) {
		t.Error("the claim was kept over a login that is not its own")
	}
}

// A claim that issued a create deletes only what is its own: a database with
// another claim's marker is skipped and the claim is released, whatever the
// policy.
func TestDatabaseDeleteSkipsAForeignDatabase(t *testing.T) {
	h := newHarness(t, common.DeletionDelete, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("swapped", nil)
	h.reconcileOK(r, db)
	h.get(db)
	if db.Status.ExternalID != createIssued {
		t.Fatalf("externalId = %q", db.Status.ExternalID)
	}
	h.fake.answer = existing("craftainer.dev/claim=other/db/uid", "-1")
	h.must(env.Client.Delete(context.Background(), db))
	h.fake.reset()
	h.reconcileOK(r, db)
	h.fake.wantNot(t, "DROP", "ALTER", "COMMENT")
	if !h.gone(db) {
		t.Error("the claim was kept over a database that is not its own")
	}
}

// A claim whose status predates the create record, with a database that carries
// its marker, is given the record, so that deleting it drops the database.
func TestDatabaseRecordsTheCreateOfAnOlderStatus(t *testing.T) {
	h := newHarness(t, common.DeletionDelete, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("older", nil)
	h.reconcileOK(r, db)
	h.get(db)
	marker := "craftainer.dev/claim=" + h.ns + "/older/" + string(db.UID)
	db.Status.ExternalID = ""
	h.must(env.Client.Status().Update(context.Background(), db))
	h.fake.answer = existing(marker, "-1")
	h.reconcileOK(r, db)
	h.get(db)
	if db.Status.ExternalID != createIssued {
		t.Errorf("externalId = %q, want the record", db.Status.ExternalID)
	}
	h.fake.reset()
	h.must(env.Client.Delete(context.Background(), db))
	h.reconcileOK(r, db)
	h.fake.want(t, `DROP DATABASE IF EXISTS "`+pgclient.DatabaseName(h.ns, "older")+`"`)
}

// A role claim records the create before the first statement, and a claim that
// issued one deletes only roles that are its own: the roles of a set that are
// another claim's are skipped, the others are dropped, and the claim is released.
func TestRoleRecordsTheCreateAndSkipsForeignRolesOnDelete(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionRetain, nil)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	// Each mix of foreign roles: the ones that are ours are dropped, the foreign
	// ones are never named by a statement, and the claim is released.
	for _, tc := range []struct {
		name    string
		foreign []int // 0 owner, 1 login a, 2 login b
		dropped func(base string) string
	}{
		{"loginb", []int{2}, func(b string) string { return `DROP ROLE IF EXISTS "` + b + `", "` + b + `_a"` }},
		{"owner", []int{0}, func(b string) string { return `DROP ROLE IF EXISTS "` + b + `_a", "` + b + `_b"` }},
		{"ownerlogina", []int{0, 1}, func(b string) string { return `DROP ROLE IF EXISTS "` + b + `_b"` }},
		{"all", []int{0, 1, 2}, nil},
	} {
		role := h.role("rec-"+tc.name, "orders", nil)
		base := pgclient.RoleBase(h.ns, role.Name)
		h.fake.answer = nil
		h.reconcileOK(rr, role)
		h.get(role)
		if role.Status.ExternalID != createIssued {
			t.Fatalf("%s: externalId = %q", tc.name, role.Status.ExternalID)
		}

		// Roles of another claim appeared since.
		claim := h.ns + "/" + role.Name + "/" + string(role.UID)
		h.fake.answer = func(sql string) []pgclient.Row {
			rows := roleAnswer(base, claim, "ReadWrite", "a", true, false)(sql)
			if len(rows) == 3 {
				for _, i := range tc.foreign {
					rows[i]["comment"] = pgclient.Marker{Claim: "other/role/uid"}.String()
				}
			}
			return rows
		}
		h.must(env.Client.Delete(ctx, role))
		h.fake.reset()
		h.reconcileOK(rr, role)
		if tc.dropped == nil {
			h.fake.wantNot(t, "DROP", "REASSIGN", "pg_terminate_backend")
		} else {
			// The whole statement: the list ends where the ours end.
			if want := tc.dropped(base); !strings.Contains(h.fake.joined()+"\n", want+"\n") {
				t.Errorf("%s: want the statement %q, got:\n%s", tc.name, want, h.fake.joined())
			}
		}
		h.fake.wantNot(t, "ALTER", "COMMENT")
		if !h.gone(role) {
			t.Errorf("%s: the claim was kept", tc.name)
		}
	}
}

// A claim whose status predates the create record, with roles that carry its
// marker, is given the record, so that deleting it drops them.
func TestRoleRecordsTheCreateOfAnOlderStatus(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	role := h.role("older", "orders", nil)
	h.reconcileOK(rr, role)
	h.get(role)
	role.Status.ExternalID = ""
	h.must(env.Client.Status().Update(context.Background(), role))
	base := pgclient.RoleBase(h.ns, "older")
	h.fake.answer = roleAnswer(base, h.ns+"/older/"+string(role.UID), "ReadWrite", "a", true, false)
	h.reconcileOK(rr, role)
	h.get(role)
	if role.Status.ExternalID != createIssued {
		t.Errorf("externalId = %q, want the record", role.Status.ExternalID)
	}
	h.fake.reset()
	h.must(env.Client.Delete(context.Background(), role))
	h.reconcileOK(rr, role)
	h.fake.want(t, `DROP ROLE IF EXISTS`)
}

// A role made by somebody else between Observe and the create is refused
// without anything being written, and the record of the create is withdrawn, so
// that deleting the claim releases it without contacting the server. A status
// write that fails stops the create before it is issued.
func TestRoleCreateRacesWithAnotherCreator(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionRetain, nil)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	role := h.role("raced", "orders", nil)
	base := pgclient.RoleBase(h.ns, "raced")

	queries := 0
	h.fake.answer = func(sql string) []pgclient.Row {
		if !strings.Contains(sql, "FROM pg_roles") {
			return nil
		}
		if queries++; queries == 1 {
			return nil // Observe: nothing there
		}
		return []pgclient.Row{{"name": base, "login": "false", "connlimit": "-1", "comment": "craftainer.dev/claim=other/role/uid"}}
	}
	h.fake.reset()
	if err := h.reconcile(rr, role); !errors.Is(err, pgclient.ErrNotOwned) {
		t.Fatalf("create: %v", err)
	}
	h.get(role)
	if role.Status.ExternalID != "" {
		t.Errorf("externalId = %q, want it withdrawn", role.Status.ExternalID)
	}
	h.fake.wantNot(t, "CREATE ROLE", "ALTER ROLE", "COMMENT", "DROP")
	h.must(env.Client.Delete(ctx, role))
	h.fake.reset()
	h.reconcileOK(rr, role)
	if h.fake.joined() != "" || !h.gone(role) {
		t.Errorf("the claim was not released without the server:\n%s", h.fake.joined())
	}

	// A failing status write stops before anything is created.
	h.fake.answer = nil
	other := h.role("unrecorded", "orders", nil)
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(err)
	boom := errors.New("status write failed")
	cfg := h.cfg
	cfg.Client = interceptor.NewClient(wc, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if r, ok := obj.(*dbv1alpha1.DatabaseRole); ok && r.Status.ExternalID == createIssued {
				return boom
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	h.fake.reset()
	if err := h.reconcile(&RoleReconciler{cfg}, other); !errors.Is(err, boom) {
		t.Errorf("status write: %v", err)
	}
	h.fake.wantNot(t, "CREATE ROLE")
}

// The External of a claim that never issued a create does nothing.
func TestNothingMade(t *testing.T) {
	ctx := context.Background()
	n := nothingMade[dbv1alpha1.DatabaseSpec, pgclient.DatabaseState]{}
	if obs, err := n.Observe(ctx, dbv1alpha1.DatabaseSpec{}); err != nil || obs.Exists {
		t.Errorf("observe: %+v, %v", obs, err)
	}
	if d, err := n.Create(ctx, dbv1alpha1.DatabaseSpec{}); d != nil || err != nil {
		t.Errorf("create: %v, %v", d, err)
	}
	if d, err := n.Update(ctx, dbv1alpha1.DatabaseSpec{}, pgclient.DatabaseState{}); d != nil || err != nil {
		t.Errorf("update: %v, %v", d, err)
	}
	if err := n.Delete(ctx, pgclient.DatabaseState{}); err != nil {
		t.Errorf("delete: %v", err)
	}
}

// The role grants on the database the Database claim reports, not one derived
// again.
func TestRoleUsesTheDatabasesExternalName(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionRetain, nil)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	db := h.database("orders", nil)
	h.reconcileOK(dbr, db)
	h.get(db)
	patch := client.MergeFrom(db.DeepCopy())
	db.Status.ExternalName = "renamed_orders"
	h.must(env.Client.Status().Patch(ctx, db, patch))

	role := h.role("app", "orders", nil)
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `|GRANT CONNECT ON DATABASE "renamed_orders" TO`, `renamed_orders|GRANT USAGE ON SCHEMA public`)
	if got := bindingSecret(h, role)["database"]; got != "renamed_orders" {
		t.Errorf("binding database = %q", got)
	}

	h.get(role)
	base := pgclient.RoleBase(h.ns, "app")
	h.fake.answer = roleAnswer(base, h.ns+"/app/"+string(role.UID), "ReadWrite", "a", true, false)
	h.must(env.Client.Delete(ctx, role))
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `renamed_orders|REASSIGN OWNED BY`)
}

// A rotation applies the grants of the spec and writes a marker that names
// them, so a privilege change cannot be recorded without having been made.
func TestRotateBeginAppliesTheGrants(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	role := h.role("rot", "orders", func(r *dbv1alpha1.DatabaseRole) { r.Spec.Privileges = dbv1alpha1.PrivilegesReadWrite })
	be, err := h.cfg.backend(context.Background(), h.ns, h.ref())
	h.must(err)
	x := newRoleExternal(be, role)
	h.fake.answer = roleAnswer(x.base, claimID(role), "ReadOnly", "a", true, false)

	data, gen, err := x.RotateBegin(context.Background(), role.Spec, pgclient.RoleState{}, lifecycle.RotationState{Active: "a"})
	h.must(err)
	if gen != "b" || string(data["username"]) != x.base+"_b" {
		t.Errorf("generation = %q, username = %q", gen, data["username"])
	}
	h.fake.want(t,
		`|ALTER ROLE "`+x.base+`_b" WITH LOGIN PASSWORD '`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO "`+x.base+`"`,
		`|COMMENT ON ROLE "`+x.base+`" IS 'craftainer.dev/claim=`+claimID(role)+` privileges=ReadWrite active=b'`)
	h.fake.wantNot(t, "PASSWORD NULL")

	h.fake.failOn = "WITH LOGIN PASSWORD"
	if _, _, err := x.RotateBegin(context.Background(), role.Spec, pgclient.RoleState{}, lifecycle.RotationState{Active: "a"}); !errors.Is(err, errStatement) {
		t.Errorf("failing rotation: %v", err)
	}
}

// What a tenant can read in a condition or an Event about a failed connection
// is one fixed text, never an address or the error the network returned.
func TestUnreachableServerShowsOnlyACategory(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &DatabaseReconciler{h.cfg}
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"refused": {fmt.Errorf("dial tcp 10.1.2.3:5432: %w", errUnreachable), pgclient.ErrConnFailed},
		"policy":  {fmt.Errorf("dial tcp 10.1.2.3:5432: %w", egress.ErrBlocked), pgclient.ErrConnPolicy},
	} {
		h.rec.Reset()
		db := h.database("quiet-"+name, nil)
		h.fake.connErr = tc.err
		_ = h.reconcile(r, db)
		h.get(db)
		msg := h.wantCondition(db.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ServerUnreachable)
		if !strings.Contains(msg, tc.want.Error()) {
			t.Errorf("%s: message %q lacks %q", name, msg, tc.want)
		}
		for _, leak := range []string{"10.1.2.3", "5432", "dial tcp"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s: %q is in the condition: %s", name, leak, msg)
			}
			h.rec.AssertNoSecret(t, leak)
		}
	}
}

// A server that cannot be asked about the role is an unreachable server.
func TestRoleOnAnUnreachableServer(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))
	role := h.role("down", "orders", nil)
	h.fake.connErr = errUnreachable
	if err := h.reconcile(&RoleReconciler{h.cfg}, role); !errors.Is(err, lifecycle.ErrUnreachable) {
		t.Errorf("reconcile: %v", err)
	}
}
