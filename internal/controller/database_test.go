package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

// existing answers the database queries as a database that exists.
func existing(claim, connLimit string, exts ...string) func(string) []pgclient.Row {
	return func(sql string) []pgclient.Row {
		switch {
		case strings.Contains(sql, "FROM pg_database"):
			return []pgclient.Row{{"marker": claim, "connlimit": connLimit}}
		case strings.Contains(sql, "FROM pg_extension"):
			var rows []pgclient.Row
			for _, e := range exts {
				rows = append(rows, pgclient.Row{"extname": e})
			}
			return rows
		}
		return nil
	}
}

func TestDatabaseLifecycleRetain(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("orders", func(d *dbv1alpha1.Database) { d.Spec.Extensions = []string{"pgcrypto"} })
	name := pgclient.DatabaseName(h.ns, "orders")

	// First reconcile: the deletion policy is pinned, the finalizer added, the
	// database created with the marker and its extension.
	h.reconcileOK(r, db)
	h.get(db)
	if db.Status.ExternalName != name {
		t.Errorf("externalName = %q, want %q", db.Status.ExternalName, name)
	}
	if db.Status.EffectiveDeletionPolicy != common.DeletionRetain || len(db.Finalizers) != 1 {
		t.Errorf("pin = %q, finalizers = %v", db.Status.EffectiveDeletionPolicy, db.Finalizers)
	}
	claim := h.ns + "/orders/" + string(db.UID)
	h.fake.want(t,
		`|CREATE DATABASE "`+name+`" TEMPLATE template0 ENCODING 'UTF8' CONNECTION LIMIT -1`,
		`|COMMENT ON DATABASE "`+name+`" IS 'craftainer.dev/claim=`+claim+`'`,
		name+`|CREATE EXTENSION IF NOT EXISTS "pgcrypto"`)
	if st, _, _ := condition(db.Status.Conditions, conditions.Ready); st != metav1.ConditionTrue {
		t.Errorf("Ready = %s", st)
	}

	// The connection came from the server: admin credentials, the default
	// port and admin database, TLS 1.2 at least, and the egress policy.
	tg := h.fake.targets[0]
	if tg.Host != "pg.example.test" || tg.Port != 5432 || tg.User != "admin" || tg.Password != "admin-pw" ||
		tg.Database != "postgres" || tg.Dial == nil {
		t.Errorf("target = %+v", tg)
	}

	// Steady state: nothing is written.
	marker := "craftainer.dev/claim=" + claim
	h.fake.answer = existing(marker, "-1", "pgcrypto", "plpgsql")
	h.fake.reset()
	h.reconcileOK(r, db)
	h.fake.wantNot(t, "CREATE", "ALTER", "COMMENT", "DROP")

	// Drift: a changed connection limit is corrected.
	limit := int32(5)
	h.must(env.Client.Get(context.Background(), client.ObjectKeyFromObject(db), db))
	patch := client.MergeFrom(db.DeepCopy())
	db.Spec.ConnectionLimit = &limit
	h.must(env.Client.Patch(context.Background(), db, patch))
	h.fake.reset()
	h.reconcileOK(r, db)
	h.fake.want(t, `|ALTER DATABASE "`+name+`" CONNECTION LIMIT 5`)

	// A database someone else owns is never touched.
	h.fake.answer = existing("craftainer.dev/claim=other/db/uid", "5", "pgcrypto")
	h.fake.reset()
	h.reconcileOK(r, db)
	h.get(db)
	h.wantCondition(db.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	h.fake.wantNot(t, "CREATE", "ALTER", "COMMENT", "DROP")

	// An extension the server does not allow is refused.
	h.fake.answer = existing(marker, "5", "pgcrypto")
	patch = client.MergeFrom(db.DeepCopy())
	db.Spec.Extensions = []string{"pgcrypto", "hstore"}
	h.must(env.Client.Patch(context.Background(), db, patch))
	h.fake.reset()
	if err := h.reconcile(r, db); !errors.Is(err, ErrExtensionNotAllowed) {
		t.Errorf("hstore: %v", err)
	}
	h.fake.wantNot(t, "CREATE", "ALTER", "DROP")

	// Deleting a Retain database leaves it in place and releases the claim,
	// even though the spec is now invalid.
	h.must(env.Client.Delete(context.Background(), db))
	h.fake.reset()
	h.reconcileOK(r, db)
	if !h.gone(db) {
		t.Error("the claim is still there")
	}
	h.fake.wantNot(t, "DROP")
	h.rec.AssertEvent(t, "Normal", conditions.EventRetained, "")
}

func TestDatabaseDeletedWithTheDeletePolicy(t *testing.T) {
	h := newHarness(t, common.DeletionDelete, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("gone", nil)
	h.reconcileOK(r, db)
	h.get(db)
	if db.Status.EffectiveDeletionPolicy != common.DeletionDelete {
		t.Fatalf("pin = %q", db.Status.EffectiveDeletionPolicy)
	}
	name := pgclient.DatabaseName(h.ns, "gone")
	h.fake.answer = existing("craftainer.dev/claim="+h.ns+"/gone/"+string(db.UID), "-1")

	h.must(env.Client.Delete(context.Background(), db))
	h.fake.reset()
	h.reconcileOK(r, db)
	h.fake.want(t, `|DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	if !h.gone(db) {
		t.Error("the claim is still there")
	}
}

// An unreachable server is a condition and a retry; deleting is blocked.
func TestDatabaseOnAnUnreachableServer(t *testing.T) {
	h := newHarness(t, common.DeletionDelete, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("down", nil)
	h.fake.connErr = errUnreachable

	if err := h.reconcile(r, db); !errors.Is(err, lifecycle.ErrUnreachable) {
		t.Errorf("reconcile: %v", err)
	}
	h.get(db)
	h.wantCondition(db.Status.Conditions, conditions.ServerReachable, metav1.ConditionFalse, conditions.ServerUnreachable)
	h.must(env.Client.Delete(context.Background(), db))
	if err := h.reconcile(r, db); err == nil {
		t.Error("the delete went through an unreachable server")
	}
	if h.gone(db) {
		t.Error("the finalizer was dropped")
	}

	// Back up: the claim is created and deleted.
	h.fake.connErr = nil
	h.reconcileOK(r, db)
	if !h.gone(db) {
		t.Error("the claim is still there")
	}
}

func TestDatabaseWithoutAServerOrANamespace(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, func(s *dbv1alpha1.ClusterDatabaseServer) {
		s.Spec.AllowedNamespaces = common.AllowedNamespaces{
			From: common.NamespacesSelector, Selector: &common.LabelSelector{MatchLabels: map[string]string{"allowed": "yes"}},
		}
	})
	r := &DatabaseReconciler{h.cfg}

	db := h.database("outside", nil)
	h.reconcileOK(r, db)
	h.get(db)
	h.wantCondition(db.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NamespaceNotAllowed)

	missing := h.database("nowhere", func(d *dbv1alpha1.Database) { d.Spec.ServerRef.Name = "no-such-server" })
	h.reconcileOK(r, missing)
	h.get(missing)
	h.wantCondition(missing.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ServerNotFound)
	if len(h.fake.targets) != 0 {
		t.Errorf("connected to %d servers", len(h.fake.targets))
	}

	// A claim that no longer exists is not an error.
	gone := &dbv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "never-created", Namespace: h.ns}}
	h.reconcileOK(r, gone)
}

// Server settings that cannot be used are reported without a connection:
// admin credentials missing, a proxy with credentials in its URL.
func TestDatabaseOnAnInvalidServer(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("invalid", nil)

	// User decision 1: the proxy URL may not carry credentials, and the
	// operator refuses to connect through one even if admission let it in.
	h.get(h.server)
	patch := client.MergeFrom(h.server.DeepCopy())
	h.server.Spec.ProxyURL = "http://proxy-user:s3cret-proxy-pw@proxy.example.test:3128"
	h.must(env.Client.Patch(context.Background(), h.server, patch))
	err := h.reconcile(r, db)
	if err == nil || !strings.Contains(err.Error(), egress.ErrProxyUserinfo.Error()) {
		t.Fatalf("proxy with userinfo: %v", err)
	}
	h.get(db)
	msg := h.wantCondition(db.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ServerUnreachable)
	for _, leak := range []string{"s3cret-proxy-pw", "proxy-user"} {
		if strings.Contains(msg, leak) || strings.Contains(err.Error(), leak) {
			t.Errorf("the proxy credentials %q leaked: %s", leak, msg)
		}
		h.rec.AssertNoSecret(t, leak)
	}
	if len(h.fake.targets) != 0 {
		t.Error("connected through a proxy with credentials")
	}

	// An admin Secret without a password.
	patch = client.MergeFrom(h.server.DeepCopy())
	h.server.Spec.ProxyURL = ""
	h.must(env.Client.Patch(context.Background(), h.server, patch))
	var admin corev1.Secret
	h.must(env.Client.Get(context.Background(), client.ObjectKey{Namespace: h.ops, Name: h.server.Spec.AdminCredentialsSecretRef.Name}, &admin))
	delete(admin.Data, AdminPasswordKey)
	h.must(env.Client.Update(context.Background(), &admin))
	err = h.reconcile(r, db)
	if !errors.Is(err, ErrAdminCredentials) {
		t.Errorf("no password: %v", err)
	}
}

// Failures reading the cluster surface as errors, not as a silent skip.
func TestDatabaseReadFailures(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	db := h.database("reads", nil)
	boom := errors.New("api server down")

	// The second read of the server (after Resolve) fails.
	gets := 0
	cfg := h.cfg
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(err)
	cfg.Client = interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*dbv1alpha1.ClusterDatabaseServer); ok {
				if gets++; gets == 2 {
					return boom
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := h.reconcile(&DatabaseReconciler{cfg}, db); !errors.Is(err, boom) {
		t.Errorf("server read: %v", err)
	}
}

// The extension rules also hold at creation, and the identifier pattern is
// checked in the controller as well as in the schema.
func TestDatabaseExtensionChecks(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("bad-ext", func(d *dbv1alpha1.Database) { d.Spec.Extensions = []string{"hstore"} })
	if err := h.reconcile(r, db); !errors.Is(err, ErrExtensionNotAllowed) {
		t.Errorf("create with hstore: %v", err)
	}
	h.fake.wantNot(t, "CREATE DATABASE")

	e := &databaseExternal{be: &backend{}}
	bad := dbv1alpha1.DatabaseSpec{Extensions: []string{`x"; DROP DATABASE y; --`}}
	if err := e.checkExtensions(bad); !errors.Is(err, pgclient.ErrInvalidExtension) {
		t.Errorf("pattern: %v", err)
	}
}

// A statement that fails is a ReconcileError, and the next reconcile retries.
func TestDatabaseStatementFailure(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &DatabaseReconciler{h.cfg}
	db := h.database("flaky", nil)
	h.fake.failOn = "CREATE DATABASE"
	if err := h.reconcile(r, db); !errors.Is(err, errStatement) {
		t.Errorf("reconcile: %v", err)
	}
	h.get(db)
	h.wantCondition(db.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ReconcileError)
	h.fake.failOn = ""
	h.reconcileOK(r, db)
	h.fake.want(t, "CREATE DATABASE")
}
