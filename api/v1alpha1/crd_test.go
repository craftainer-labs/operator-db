package v1alpha1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// These tests install the generated CRDs into envtest's kube-apiserver and
// check the schema defaults and the OpenAPI and CEL rules there. They need the
// binaries scripts/go-coverage.sh and `make test` fetch with setup-envtest
// (KUBEBUILDER_ASSETS).

var k8sClient client.Client

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start envtest:", err)
		os.Exit(1)
	}
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := AddToScheme(s); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create a client:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot stop envtest:", err)
		code = 1
	}
	os.Exit(code)
}

var ctx = context.Background()

func ref() common.ServerRef {
	return common.ServerRef{Kind: KindClusterDatabaseServer, Name: "shared"}
}

func clusterServerObj(name string) *ClusterDatabaseServer {
	s := &ClusterDatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: name}}
	s.Spec.Host = "pg.example.svc"
	s.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: "admin", Namespace: "operator-db-system"}
	s.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	return s
}

func serverObj(name string) *DatabaseServer {
	s := &DatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	s.Spec.Host = "pg.example.svc"
	s.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: "admin"}
	return s
}

func databaseObj(name string) *Database {
	d := &Database{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	d.Spec.ServerRef = ref()
	return d
}

func roleObj(name string) *DatabaseRole {
	r := &DatabaseRole{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	r.Spec.ServerRef = ref()
	r.Spec.DatabaseRef = DatabaseRef{Name: "orders"}
	return r
}

// invalid expects the API server to reject obj with a message containing want.
func invalid(t *testing.T, obj client.Object, want string) {
	t.Helper()
	g := NewWithT(t)
	err := k8sClient.Create(ctx, obj)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(want))
}

func TestServerDefaultsAndRules(t *testing.T) {
	g := NewWithT(t)

	cs := clusterServerObj("defaults")
	g.Expect(k8sClient.Create(ctx, cs)).To(Succeed())
	g.Expect(cs.Spec.Port).To(Equal(DefaultPort))
	g.Expect(cs.Spec.AdminDatabase).To(Equal("postgres"))
	g.Expect(cs.Spec.SSLMode).To(Equal(SSLModeVerifyFull))
	g.Expect(cs.Spec.AllowedExtensions).NotTo(BeNil())

	ns := serverObj("defaults")
	g.Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	g.Expect(ns.Spec.Port).To(Equal(DefaultPort))

	// A cluster server needs the operator namespace for its admin Secret, and
	// allowedNamespaces.
	noNS := clusterServerObj("no-ns")
	noNS.Spec.AdminCredentialsSecretRef.Namespace = ""
	invalid(t, noNS, "adminCredentialsSecretRef.namespace is required")

	noAllowed := clusterServerObj("no-allowed")
	noAllowed.Spec.AllowedNamespaces = common.AllowedNamespaces{}
	invalid(t, noAllowed, "allowedNamespaces")

	// An empty selector would match everything; from: All says that.
	empty := clusterServerObj("empty-selector")
	empty.Spec.AllowedNamespaces = common.AllowedNamespaces{
		From: common.NamespacesSelector, Selector: &common.LabelSelector{},
	}
	invalid(t, empty, "selector must have matchLabels or matchExpressions")

	both := clusterServerObj("all-with-selector")
	both.Spec.AllowedNamespaces.Selector = &common.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
	invalid(t, both, "selector must not be set when from is All")

	// A namespaced server's Secret is in its own namespace.
	withNS := serverObj("with-ns")
	withNS.Spec.AdminCredentialsSecretRef.Namespace = "other"
	invalid(t, withNS, "adminCredentialsSecretRef.namespace is forbidden")

	weak := serverObj("weak-ssl")
	weak.Spec.SSLMode = "disable"
	invalid(t, weak, "sslMode")

	badPort := serverObj("bad-port")
	badPort.Spec.Port = 70000
	invalid(t, badPort, "port")

	noHost := serverObj("no-host")
	noHost.Spec.Host = ""
	invalid(t, noHost, "host")

	badTLS := serverObj("bad-tls")
	badTLS.Spec.TLS = &common.TLS{CABundle: &common.CABundle{}}
	invalid(t, badTLS, "exactly one of secretRef and configMapRef")
}

func TestDatabaseDefaultsAndRules(t *testing.T) {
	g := NewWithT(t)

	d := databaseObj("orders")
	d.Spec.Extensions = []string{"pgcrypto"}
	g.Expect(k8sClient.Create(ctx, d)).To(Succeed())
	g.Expect(d.Spec.Encoding).To(Equal("UTF8"))
	g.Expect(d.Spec.ConnectionLimit).NotTo(BeNil())
	g.Expect(*d.Spec.ConnectionLimit).To(Equal(int32(-1)))
	g.Expect(d.Spec.ManagementPolicies).To(Equal([]common.ManagementPolicy{common.ManagementAll}))
	// deletionPolicy has no schema default: the operator pins it.
	g.Expect(d.Spec.DeletionPolicy).To(BeEmpty())

	// A limit of zero is kept, not defaulted away.
	zero := int32(0)
	z := databaseObj("zero-limit")
	z.Spec.ConnectionLimit = &zero
	g.Expect(k8sClient.Create(ctx, z)).To(Succeed())
	g.Expect(*z.Spec.ConnectionLimit).To(Equal(int32(0)))

	// serverRef and locale are immutable.
	d.Spec.ServerRef.Name = "other"
	err := k8sClient.Update(ctx, d)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serverRef is immutable"))

	loc := databaseObj("with-locale")
	loc.Spec.Locale = "en_US.UTF-8"
	g.Expect(k8sClient.Create(ctx, loc)).To(Succeed())
	loc.Spec.Locale = "C"
	err = k8sClient.Update(ctx, loc)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("locale is immutable"))

	dotted := databaseObj("has.dot")
	invalid(t, dotted, "metadata.name must be lower-case alphanumerics")

	badKind := databaseObj("bad-kind")
	badKind.Spec.ServerRef.Kind = "Secret"
	invalid(t, badKind, "serverRef.kind must be ClusterDatabaseServer or DatabaseServer")

	dup := databaseObj("dup-ext")
	dup.Spec.Extensions = []string{"pgcrypto", "pgcrypto"}
	invalid(t, dup, "Duplicate value")

	enc := databaseObj("bad-encoding")
	enc.Spec.Encoding = "LATIN1"
	invalid(t, enc, "encoding")

	badLocale := databaseObj("bad-locale")
	badLocale.Spec.Locale = "en US"
	invalid(t, badLocale, "locale")

	low := int32(-2)
	lim := databaseObj("low-limit")
	lim.Spec.ConnectionLimit = &low
	invalid(t, lim, "connectionLimit")

	star := databaseObj("star-and-more")
	star.Spec.ManagementPolicies = []common.ManagementPolicy{common.ManagementAll, common.ManagementObserve}
	invalid(t, star, "'*' must stand alone")

	noObserve := databaseObj("no-observe")
	noObserve.Spec.ManagementPolicies = []common.ManagementPolicy{common.ManagementCreate}
	invalid(t, noObserve, "an explicit list must contain Observe")

	retain := databaseObj("retain")
	retain.Spec.DeletionPolicy = common.DeletionRetain
	g.Expect(k8sClient.Create(ctx, retain)).To(Succeed())
}

func TestDatabaseRoleDefaultsAndRules(t *testing.T) {
	g := NewWithT(t)

	r := roleObj("orders-app")
	g.Expect(k8sClient.Create(ctx, r)).To(Succeed())
	g.Expect(r.Spec.Privileges).To(Equal(PrivilegesReadWrite))
	g.Expect(*r.Spec.ConnectionLimit).To(Equal(int32(-1)))

	// databaseRef is immutable.
	r.Spec.DatabaseRef.Name = "other"
	err := k8sClient.Update(ctx, r)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("databaseRef.name is immutable"))

	badKind := roleObj("bad-kind")
	badKind.Spec.ServerRef.Kind = "Pod"
	invalid(t, badKind, "serverRef.kind must be ClusterDatabaseServer or DatabaseServer")

	priv := roleObj("bad-privileges")
	priv.Spec.Privileges = "Superuser"
	invalid(t, priv, "privileges")

	noRef := roleObj("no-ref")
	noRef.Spec.DatabaseRef.Name = ""
	invalid(t, noRef, "databaseRef")

	// Rotation needs the Update management policy.
	interval := metav1.Duration{Duration: 720 * 3600 * 1e9}
	rot := roleObj("rotation-without-update")
	rot.Spec.RotationPolicy = &common.RotationPolicy{Interval: &interval}
	rot.Spec.ManagementPolicies = []common.ManagementPolicy{common.ManagementObserve, common.ManagementCreate}
	invalid(t, rot, "rotationPolicy needs the Update management policy")

	okRot := roleObj("rotation")
	okRot.Spec.RotationPolicy = &common.RotationPolicy{Interval: &interval}
	g.Expect(k8sClient.Create(ctx, okRot)).To(Succeed())
	g.Expect(okRot.Spec.RotationPolicy.GracePeriod).NotTo(BeNil())
	g.Expect(okRot.Spec.RotationPolicy.GracePeriod.Duration.String()).To(Equal("1h0m0s"))

	short := metav1.Duration{Duration: 30 * 60 * 1e9}
	tooShort := roleObj("short-interval")
	tooShort.Spec.RotationPolicy = &common.RotationPolicy{Interval: &short}
	invalid(t, tooShort, "interval must be between 1h and 8760h")
}
