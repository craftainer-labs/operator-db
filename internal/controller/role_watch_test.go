package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
)

// listStub answers List with a fixed set of roles and records the options.
type listStub struct {
	client.Reader
	items []dbv1alpha1.DatabaseRole
	err   error
	opts  *client.ListOptions
}

func (l *listStub) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if l.err != nil {
		return l.err
	}
	l.opts = &client.ListOptions{}
	l.opts.ApplyOptions(opts)
	list.(*dbv1alpha1.DatabaseRoleList).Items = l.items
	return nil
}

func TestIndexDatabaseRef(t *testing.T) {
	r := &dbv1alpha1.DatabaseRole{}
	r.Spec.DatabaseRef.Name = "orders"
	if got := indexDatabaseRef(r); len(got) != 1 || got[0] != "orders" {
		t.Errorf("index = %v", got)
	}
}

// A Database event reaches the roles of its namespace that name it.
func TestRolesOfDatabase(t *testing.T) {
	ctx := context.Background()
	mk := func(name string) dbv1alpha1.DatabaseRole {
		return dbv1alpha1.DatabaseRole{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"}}
	}
	stub := &listStub{items: []dbv1alpha1.DatabaseRole{mk("one"), mk("two")}}
	db := &dbv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "tenant"}}

	reqs := rolesOfDatabase(stub)(ctx, db)
	if len(reqs) != 2 || reqs[0].Name != "one" || reqs[1].Namespace != "tenant" {
		t.Errorf("requests = %v", reqs)
	}
	if stub.opts.Namespace != "tenant" || stub.opts.FieldSelector.String() != databaseRefIndex+"=orders" {
		t.Errorf("list options = %+v", stub.opts)
	}

	stub.err = errStatement
	if reqs := rolesOfDatabase(stub)(ctx, db); len(reqs) != 0 {
		t.Errorf("a failed list gave %v", reqs)
	}
}

// failingIndexer and indexFailure make the registration of the index fail.
type failingIndexer struct{}

func (failingIndexer) IndexField(context.Context, client.Object, string, client.IndexerFunc) error {
	return errStatement
}

type indexFailure struct{ ctrl.Manager }

func (indexFailure) GetFieldIndexer() client.FieldIndexer { return failingIndexer{} }

func TestRoleSetupReportsAnIndexError(t *testing.T) {
	if err := (&RoleReconciler{}).SetupWithManager(indexFailure{}); !errors.Is(err, errStatement) {
		t.Errorf("setup: %v", err)
	}
}
