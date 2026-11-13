package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
)

func clusterServer(host, proxy string) *dbv1alpha1.ClusterDatabaseServer {
	s := &dbv1alpha1.ClusterDatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "pg"}}
	s.Spec.Host = host
	s.Spec.ProxyURL = proxy
	s.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: "pg-admin", Namespace: "operator-db-system"}
	s.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	return s
}

func namespacedServer(host, proxy string) *dbv1alpha1.DatabaseServer {
	s := &dbv1alpha1.DatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "default"}}
	s.Spec.Host = host
	s.Spec.ProxyURL = proxy
	s.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: "pg-admin"}
	return s
}

func TestValidateServer(t *testing.T) {
	g := NewWithT(t)
	for _, host := range []string{"pg", "pg.data.svc.cluster.local", "PG.Example.COM", "10.0.0.1", "::1", "2001:db8::1"} {
		g.Expect(validateServer(host, "")).To(Succeed(), host)
	}
	for _, host := range []string{"", "pg_1", "-pg", "pg..x", "[::1]", "pg:5432", "pg/x", "a b"} {
		g.Expect(validateServer(host, "")).To(MatchError(ErrInvalidHost), host)
	}
	for _, proxy := range []string{"http://proxy:3128", "https://proxy", "socks5://127.0.0.1:1080"} {
		g.Expect(validateServer("pg", proxy)).To(Succeed(), proxy)
	}
	for _, proxy := range []string{"ftp://proxy", "proxy:3128", "http://", "://x", "http://%zz", "pg",
		"http://user:s3cret@proxy:3128", "socks5://user@proxy", "https://:pw@proxy"} {
		g.Expect(validateServer("pg", proxy)).To(MatchError(ErrInvalidProxyURL), proxy)
	}
	// The proxy URL, which may hold credentials, is not echoed.
	g.Expect(validateServer("pg", "ftp://user:s3cret@x").Error()).NotTo(ContainSubstring("s3cret"))
	// Every violation is reported, not only the first.
	err := validateServer("a b", "ftp://x")
	g.Expect(err).To(MatchError(ErrInvalidHost))
	g.Expect(err).To(MatchError(ErrInvalidProxyURL))
}

func TestClusterDatabaseServerValidator(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	v := &ClusterDatabaseServerValidator{}

	warnings, err := v.ValidateCreate(ctx, clusterServer("pg", ""))
	g.Expect(warnings).To(BeEmpty())
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateCreate(ctx, clusterServer("a b", ""))
	g.Expect(err).To(MatchError(ErrInvalidHost))

	_, err = v.ValidateUpdate(ctx, clusterServer("pg", ""), clusterServer("pg2", ""))
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateUpdate(ctx, clusterServer("pg", ""), clusterServer("pg", "ftp://x"))
	g.Expect(err).To(MatchError(ErrInvalidProxyURL))

	// An object being deleted is always admitted, so finalizer removal is
	// never blocked.
	deleting := clusterServer("a b", "")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err = v.ValidateUpdate(ctx, clusterServer("pg", ""), deleting)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = v.ValidateDelete(ctx, clusterServer("", ""))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestDatabaseServerValidator(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	v := &DatabaseServerValidator{}

	warnings, err := v.ValidateCreate(ctx, namespacedServer("pg", ""))
	g.Expect(warnings).To(BeEmpty())
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateCreate(ctx, namespacedServer("a b", ""))
	g.Expect(err).To(MatchError(ErrInvalidHost))

	_, err = v.ValidateUpdate(ctx, namespacedServer("pg", ""), namespacedServer("pg2", ""))
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateUpdate(ctx, namespacedServer("pg", ""), namespacedServer("pg", "ftp://x"))
	g.Expect(err).To(MatchError(ErrInvalidProxyURL))

	deleting := namespacedServer("a b", "")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err = v.ValidateUpdate(ctx, namespacedServer("pg", ""), deleting)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = v.ValidateDelete(ctx, namespacedServer("", ""))
	g.Expect(err).NotTo(HaveOccurred())
}

// The same rules, reached through the API server and the served webhooks.
var _ = Describe("Server webhooks", func() {
	It("admits a valid ClusterDatabaseServer and rejects a bad host or proxy", func() {
		ok := clusterServer("pg.data.svc", "")
		ok.Name = "valid"
		Expect(k8sClient.Create(ctx, ok)).To(Succeed())

		badHost := clusterServer("a b", "")
		badHost.Name = "bad-host"
		err := k8sClient.Create(ctx, badHost)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(ErrInvalidHost.Error()))

		ok.Spec.ProxyURL = "ftp://proxy"
		err = k8sClient.Update(ctx, ok)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(ErrInvalidProxyURL.Error()))
		Expect(k8sClient.Delete(ctx, ok)).To(Succeed())
	})

	It("admits a valid DatabaseServer and rejects a bad host or proxy", func() {
		ok := namespacedServer("pg.team-a.svc", "")
		ok.Name = "valid"
		Expect(k8sClient.Create(ctx, ok)).To(Succeed())

		badProxy := namespacedServer("pg", "proxy:3128")
		badProxy.Name = "bad-proxy"
		err := k8sClient.Create(ctx, badProxy)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(ErrInvalidProxyURL.Error()))

		ok.Spec.Host = "a b"
		err = k8sClient.Update(ctx, ok)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(ErrInvalidHost.Error()))
		Expect(k8sClient.Delete(ctx, ok)).To(Succeed())
	})
})
