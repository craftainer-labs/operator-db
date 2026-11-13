package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// A namespaced server is read from the claim's namespace, and a cluster one
// by name.
func TestServerSpecOfBothKinds(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	ns := &dbv1alpha1.DatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: h.ns}}
	ns.Spec.Host = "local.example.test"
	ns.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: "local-admin"}
	h.must(env.Client.Create(context.Background(), ns))

	got, err := h.cfg.serverSpec(context.Background(), h.ns, common.ServerRef{Kind: dbv1alpha1.KindDatabaseServer, Name: "local"})
	if err != nil || got.Host != "local.example.test" {
		t.Errorf("namespaced: %+v, %v", got, err)
	}
	got, err = h.cfg.serverSpec(context.Background(), h.ns, h.ref())
	if err != nil || got.Host != "pg.example.test" {
		t.Errorf("cluster: %+v, %v", got, err)
	}
	if _, err = h.cfg.serverSpec(context.Background(), h.ns, common.ServerRef{Kind: dbv1alpha1.KindDatabaseServer, Name: "absent"}); err == nil {
		t.Error("a missing namespaced server was found")
	}
}

func TestConnLimit(t *testing.T) {
	five := int32(5)
	if connLimit(nil) != -1 || connLimit(&five) != 5 {
		t.Errorf("connLimit = %d, %d", connLimit(nil), connLimit(&five))
	}
}

// The schema defaults spec.privileges, so a claim read from the API server
// never reaches the fallback; one built in memory does.
func TestNewRoleExternalDefaults(t *testing.T) {
	claim := &dbv1alpha1.DatabaseRole{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant"}}
	claim.Spec.DatabaseRef.Name = "orders"
	e := newRoleExternal(&backend{}, claim)
	if e.privileges != pgclient.ReadWrite || e.limit != -1 || claim.GetExternalName() != e.base || e.base == "" {
		t.Errorf("external = %+v", e)
	}
	// A name already in status is kept, and an explicit privilege is used.
	claim.SetExternalName("kept")
	claim.Spec.Privileges = dbv1alpha1.PrivilegesOwner
	e = newRoleExternal(&backend{}, claim)
	if e.base != "kept" || e.privileges != string(dbv1alpha1.PrivilegesOwner) {
		t.Errorf("external = %+v", e)
	}
}

func TestActiveOf(t *testing.T) {
	if activeOf(pgclient.Marker{}) != pgclient.GenA || activeOf(pgclient.Marker{Active: pgclient.GenB}) != pgclient.GenB {
		t.Error("activeOf")
	}
}
