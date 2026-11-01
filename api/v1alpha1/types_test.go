package v1alpha1

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"
)

// The kinds satisfy the interfaces lib-operator-go's reconciler and server
// resolver are written against.
var (
	_ lifecycle.Claim = (*Database)(nil)
	_ lifecycle.Claim = (*DatabaseRole)(nil)
	_ server.Server   = (*ClusterDatabaseServer)(nil)
	_ server.Server   = (*DatabaseServer)(nil)
)

func TestAddToScheme(t *testing.T) {
	g := NewWithT(t)
	s := runtime.NewScheme()
	g.Expect(AddToScheme(s)).To(Succeed())
	for _, kind := range []string{
		KindClusterDatabaseServer, KindDatabaseServer, "Database", "DatabaseRole",
		"ClusterDatabaseServerList", "DatabaseServerList", "DatabaseList", "DatabaseRoleList",
	} {
		g.Expect(s.Recognizes(SchemeGroupVersion.WithKind(kind))).To(BeTrue(), kind)
	}
	g.Expect(GroupVersion.Group).To(Equal("db.craftainer.dev"))
}

func TestServerAccessors(t *testing.T) {
	g := NewWithT(t)

	cluster := &ClusterDatabaseServer{}
	cluster.Spec.Host = "pg.example.svc"
	cluster.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	cluster.Spec.ProxyURL = "http://proxy:3128"
	g.Expect(cluster.GetCommon().ProxyURL).To(Equal("http://proxy:3128"))
	g.Expect(cluster.GetAllowedNamespaces().From).To(Equal(common.NamespacesAll))
	// An unset port is the default; a set one is used.
	g.Expect(cluster.GetEndpoint()).To(Equal("pg.example.svc:5432"))
	cluster.Spec.Port = 6432
	g.Expect(cluster.GetEndpoint()).To(Equal("pg.example.svc:6432"))

	namespaced := &DatabaseServer{}
	namespaced.Spec.Host = "::1"
	namespaced.Spec.Port = 5433
	namespaced.Spec.ProxyURL = "socks5://p:1080"
	g.Expect(namespaced.GetCommon().ProxyURL).To(Equal("socks5://p:1080"))
	g.Expect(namespaced.GetAllowedNamespaces()).To(BeNil())
	g.Expect(namespaced.GetEndpoint()).To(Equal("[::1]:5433"))
}

func TestDatabaseAccessors(t *testing.T) {
	g := NewWithT(t)
	d := &Database{ObjectMeta: metav1.ObjectMeta{Name: "orders"}}
	d.Spec.ServerRef = common.ServerRef{Kind: KindClusterDatabaseServer, Name: "shared"}
	d.Spec.ManagementPolicies = []common.ManagementPolicy{common.ManagementObserve}
	d.Spec.DeletionPolicy = common.DeletionRetain

	g.Expect(d.GetServerRef()).To(Equal(common.ServerRef{Kind: KindClusterDatabaseServer, Name: "shared"}))
	g.Expect(d.GetManagementPolicies()).To(Equal([]common.ManagementPolicy{common.ManagementObserve}))
	g.Expect(d.GetDeletionPolicy()).To(Equal(common.DeletionRetain))
	*d.GetConditions() = append(*d.GetConditions(), metav1.Condition{Type: "Ready"})
	g.Expect(d.Status.Conditions).To(HaveLen(1))

	// A data kind has no binding and does not rotate.
	g.Expect(d.GetBindingSecretName()).To(BeEmpty())
	d.SetBindingName("ignored")
	g.Expect(d.GetRotationPolicy()).To(BeNil())
	d.SetRotationStatus(common.RotationStatus{Sequence: 3})
	g.Expect(d.GetRotationStatus()).To(Equal(common.RotationStatus{}))

	d.SetExternalName("team-a_orders")
	d.SetExternalID("17")
	d.SetObservedGeneration(4)
	now := metav1.NewTime(time.Unix(1700000000, 0))
	d.SetLastReconcileTime(now)
	d.SetLateInitialized(map[string]string{"owner": "x"})
	d.SetEffectiveDeletionPolicy(common.DeletionDelete, common.SourceOperatorDefault)
	g.Expect(d.GetExternalName()).To(Equal("team-a_orders"))
	g.Expect(d.GetExternalID()).To(Equal("17"))
	g.Expect(d.Status.ObservedGeneration).To(Equal(int64(4)))
	g.Expect(d.Status.LastReconcileTime.Equal(&now)).To(BeTrue())
	g.Expect(d.Status.LateInitialized).To(Equal(map[string]string{"owner": "x"}))
	g.Expect(d.GetEffectiveDeletionPolicy()).To(Equal(common.DeletionDelete))
	g.Expect(d.Status.DeletionPolicySource).To(Equal(common.SourceOperatorDefault))
}

func TestDatabaseRoleAccessors(t *testing.T) {
	g := NewWithT(t)
	r := &DatabaseRole{ObjectMeta: metav1.ObjectMeta{Name: "orders-app"}}
	r.Spec.ServerRef = common.ServerRef{Kind: KindDatabaseServer, Name: "team"}
	r.Spec.ManagementPolicies = []common.ManagementPolicy{common.ManagementAll}

	g.Expect(r.GetServerRef().Name).To(Equal("team"))
	g.Expect(r.GetManagementPolicies()).To(Equal([]common.ManagementPolicy{common.ManagementAll}))
	*r.GetConditions() = append(*r.GetConditions(), metav1.Condition{Type: "Synced"})
	g.Expect(r.Status.Conditions).To(HaveLen(1))

	// A credential kind has no deletion policy.
	g.Expect(r.GetDeletionPolicy()).To(BeEmpty())
	r.SetEffectiveDeletionPolicy(common.DeletionDelete, common.SourceFallback)
	g.Expect(r.GetEffectiveDeletionPolicy()).To(BeEmpty())

	// The binding Secret is named after the claim until the spec names it.
	g.Expect(r.GetBindingSecretName()).To(Equal("orders-app"))
	r.Spec.Binding.SecretName = "orders-db"
	g.Expect(r.GetBindingSecretName()).To(Equal("orders-db"))
	r.SetBindingName("orders-db")
	g.Expect(r.Status.Binding.Name).To(Equal("orders-db"))

	g.Expect(r.GetRotationPolicy()).To(BeNil())
	r.Spec.RotationPolicy = &common.RotationPolicy{}
	g.Expect(r.GetRotationPolicy()).To(BeIdenticalTo(r.Spec.RotationPolicy))
	g.Expect(r.GetRotationStatus()).To(Equal(common.RotationStatus{}))
	r.SetRotationStatus(common.RotationStatus{Active: "a", Sequence: 2})
	g.Expect(r.GetRotationStatus()).To(Equal(common.RotationStatus{Active: "a", Sequence: 2}))

	r.SetExternalName("team-a_orders-app")
	r.SetExternalID("9")
	r.SetObservedGeneration(2)
	now := metav1.NewTime(time.Unix(1700000000, 0))
	r.SetLastReconcileTime(now)
	r.SetLateInitialized(map[string]string{"k": "v"})
	g.Expect(r.GetExternalName()).To(Equal("team-a_orders-app"))
	g.Expect(r.GetExternalID()).To(Equal("9"))
	g.Expect(r.Status.ObservedGeneration).To(Equal(int64(2)))
	g.Expect(r.Status.LastReconcileTime.Equal(&now)).To(BeTrue())
	g.Expect(r.Status.LateInitialized).To(Equal(map[string]string{"k": "v"}))
}
