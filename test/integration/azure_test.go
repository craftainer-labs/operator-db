//go:build integration && azure

package integration

// The Azure backend in Password mode (NKdb-3a) against a fake of Azure Resource
// Manager (servers.get) and the Entra authority, and the TLS PostgreSQL of the
// other tests as the "Azure" data plane. Resource Manager has no emulator, so
// the fake is a small Go test server in this process. azcore and MSAL refuse
// plain http for authenticated requests, so it serves TLS with a certificate
// generated here for the private address it listens on, which the operator
// trusts through Config.CloudRootCAs. The address is private, never loopback,
// because pkg/egress blocks loopback in both modes; EGRESS_MODE=allow-private is
// in this test's wiring only. Nothing validates an Entra token here
// (docs/nfrs/NFR-0005).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	libserver "github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/controller"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

const (
	azOpsNS  = "operator-db-system"
	azTenant = "00000000-0000-4000-8000-000000000001"
	azSub    = "00000000-0000-4000-8000-000000000002"
	azGroup  = "rg-orders"
	azFQDN   = "orders.postgres.database.azure.com"
	// azTLSServer is a name in the test certificate; the address is a bare IP.
	azTLSServer = "postgres"
)

// azPrivateAddress is an IPv4 address of this host that is neither loopback nor
// link-local.
func azPrivateAddress(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip := n.IP.To4(); ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return ip.String()
			}
		}
	}
	t.Fatal("this host has no address that is not loopback")
	return ""
}

// azureFake is Azure Resource Manager and the Entra authority.
type azureFake struct {
	srv   *httptest.Server
	roots *x509.CertPool
	// missing makes servers.get answer 404.
	missing bool
}

func newAzureFake(t *testing.T) *azureFake {
	t.Helper()
	ip := azPrivateAddress(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "azure-fake"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(ip)}, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f := &azureFake{roots: x509.NewCertPool()}
	f.roots.AddCert(leaf)
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	f.srv.Listener = l
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *azureFake) handle(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
		base := f.srv.URL + strings.TrimSuffix(r.URL.Path, "/v2.0/.well-known/openid-configuration")
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": base + "/v2.0",
			"authorization_endpoint": base + "/oauth2/v2.0/authorize", "token_endpoint": base + "/oauth2/v2.0/token"})
	case strings.HasSuffix(r.URL.Path, "/token"):
		_, _ = fmt.Fprint(w, `{"access_token":"fake-entra-token","token_type":"Bearer","expires_in":3600,"ext_expires_in":3600}`)
	case strings.HasSuffix(r.URL.Path, "/flexibleServers/orders") && !f.missing:
		_ = json.NewEncoder(w).Encode(map[string]any{"id": r.URL.Path, "name": "orders", "location": "westeurope",
			"properties": map[string]string{"state": "Ready", "fullyQualifiedDomainName": azFQDN}})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"error":{"code":"ResourceNotFound"}}`)
	}
}

type azureHarness struct {
	cfg  controller.Config
	conn libserver.Connection
	pg   server
	fake *azureFake
}

func newAzureHarness(t *testing.T, mode egress.Mode) *azureHarness {
	t.Helper()
	pg := load(t)
	f := newAzureFake(t)
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: azOpsNS},
		Data: map[string][]byte{cloud.KeyAzureClientID: []byte("11111111-1111-4111-8111-111111111111"),
			cloud.KeyAzureClientSecret: []byte("entra-client-secret")},
	}
	h := &azureHarness{pg: pg, fake: f}
	h.cfg = controller.Config{
		Client: fake.NewClientBuilder().WithObjects(creds).Build(), OperatorNamespace: azOpsNS,
		Egress: egress.NewPolicy(mode), EgressMode: mode, ARM: controller.NewARM, CloudRootCAs: f.roots, Connect: pgclient.Connect,
	}
	h.conn = libserver.Connection{
		Admin:    map[string][]byte{controller.AdminUsernameKey: []byte(pg.user), controller.AdminPasswordKey: []byte(pg.password)},
		CABundle: pg.ca, ServerName: azTLSServer, MinTLSVersion: 0x0304,
	}
	return h
}

// server is a cluster server for the flexible server name, pointing at the fake
// and, as the data plane, at the PostgreSQL (spec.host overrides the host name
// the fake reports, which does not resolve).
func (h *azureHarness) server(name, override string) *dbv1alpha1.ClusterDatabaseServer {
	s := &dbv1alpha1.ClusterDatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1}}
	s.Spec.Host, s.Spec.Port, s.Spec.AdminDatabase = h.pg.host, int32(h.pg.port), "postgres"
	s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderAzure, Azure: &dbv1alpha1.ClusterAzureDatabase{
		AzureClusterBlock: cloud.AzureClusterBlock{
			AzureBlock:       cloud.AzureBlock{TenantID: azTenant, SubscriptionID: azSub, ResourceGroup: azGroup},
			EndpointOverride: override},
		AzureTarget: dbv1alpha1.AzureTarget{ServerName: name},
	}}
	s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "azure", Namespace: azOpsNS}}
	s.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	return s
}

func TestAzureProbeDiscoversAndConnects(t *testing.T) {
	h := newAzureHarness(t, egress.ModeAllowPrivate)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	srv := h.server("orders", h.fake.srv.URL)
	version, err := controller.Prober{Config: h.cfg}.Probe(ctx, srv, h.conn)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(version, "18") {
		t.Errorf("server_version = %q", version)
	}
	d := srv.Status.Discovered
	if d[dbv1alpha1.DiscoveredEndpoint] != azFQDN || d[dbv1alpha1.DiscoveredPort] != "5432" ||
		d[dbv1alpha1.DiscoveredRegion] != "westeurope" || !strings.HasSuffix(d[dbv1alpha1.DiscoveredResourceID], "/flexibleServers/orders") {
		t.Errorf("discovered = %v", d)
	}
	if c := meta.FindStatusCondition(srv.Status.Conditions, controller.ConditionCloudResolved); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("CloudResolved = %+v", c)
	}
}

func TestAzureProbeFailuresAreCategories(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// A server the fake does not have.
	h := newAzureHarness(t, egress.ModeAllowPrivate)
	h.fake.missing = true
	_, err := controller.Prober{Config: h.cfg}.Probe(ctx, h.server("orders", h.fake.srv.URL), h.conn)
	var ce *controller.CloudError
	if !errors.As(err, &ce) || ce.Reason != controller.ReasonTargetNotFound {
		t.Errorf("missing server: %v", err)
	}

	// endpointOverride is refused in strict mode, before any request is made.
	strict := newAzureHarness(t, egress.ModeStrict)
	_, err = controller.Prober{Config: strict.cfg}.Probe(ctx, strict.server("orders", strict.fake.srv.URL), strict.conn)
	if !errors.As(err, &ce) || ce.Reason != controller.ReasonEndpointOverrideForbidden {
		t.Errorf("override in strict mode: %v", err)
	}
}
