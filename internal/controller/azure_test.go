package controller

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azcloud "github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
)

const (
	azTenant = "00000000-0000-4000-8000-000000000001"
	azSub    = "00000000-0000-4000-8000-000000000002"
	azGroup  = "rg-orders"
	azFQDN   = "orders.postgres.database.azure.com"
	// azSecretText is in the answers of the fake and must reach no error,
	// condition or Event.
	azSecretText = "rg-orders-secret-name"
)

// armServerPath is the resource path of the server the fake knows.
const armServerPath = "/subscriptions/" + azSub + "/resourceGroups/" + azGroup + "/providers/Microsoft.DBforPostgreSQL/flexibleServers/orders"

// recordedAzureServer is the shape of an answer of servers.get (fields the
// operator does not read are left out).
const recordedAzureServer = `{
  "id": "` + armServerPath + `",
  "name": "orders",
  "type": "Microsoft.DBforPostgreSQL/flexibleServers",
  "location": "West Europe",
  "sku": {"name": "Standard_D2ds_v5", "tier": "GeneralPurpose"},
  "properties": {"state": "Ready", "version": "16", "fullyQualifiedDomainName": "` + azFQDN + `", "administratorLogin": "pgadmin"}
}`

// fakeARM is Azure Resource Manager (servers.get) and the Entra authority, as
// one TLS server: MSAL and azcore both need https. The operator reaches it the
// way it reaches the real ones, through cloud.Egress.Azure with a client secret
// and endpointOverride (the Policy of the tests blocks nothing, so loopback
// stands for a private address).
type fakeARM struct {
	srv    *httptest.Server
	roots  *x509.CertPool
	mu     sync.Mutex
	reqs   []string
	authz  []string
	server string
	// status forces an HTTP status for servers.get; entra forces one for the
	// Entra token endpoint.
	status, entra int
}

func newFakeARM(t *testing.T) *fakeARM {
	t.Helper()
	f := &fakeARM{server: recordedAzureServer}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	f.roots = x509.NewCertPool()
	f.roots.AddCert(f.srv.Certificate())
	return f
}

func (f *fakeARM) handle(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
		base := f.srv.URL + strings.TrimSuffix(r.URL.Path, "/v2.0/.well-known/openid-configuration")
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": base + "/v2.0",
			"authorization_endpoint": base + "/oauth2/v2.0/authorize", "token_endpoint": base + "/oauth2/v2.0/token"})
	case strings.HasSuffix(r.URL.Path, "/token"):
		if f.entra != 0 {
			w.WriteHeader(f.entra)
			_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"`+azSecretText+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"fake-entra-token","token_type":"Bearer","expires_in":3600,"ext_expires_in":3600}`)
	case strings.Contains(r.URL.Path, "/flexibleServers/"):
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.authz = append(f.authz, r.Header.Get("Authorization"))
		switch {
		case f.status != 0:
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, `{"error":{"code":"Failed","message":"`+azSecretText+`"}}`)
		case r.URL.Path != armServerPath:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"ResourceNotFound","message":"`+azSecretText+`"}}`)
		default:
			_, _ = io.WriteString(w, f.server)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeARM) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

// serverWith is the recorded server with a replacement of text.
func serverWith(old, repl string) string { return strings.Replace(recordedAzureServer, old, repl, 1) }

// azureHarness is a harness whose server is an Azure one, with the fake as its
// Resource Manager and Entra authority.
func azureHarness(t *testing.T, f *fakeARM) *harness {
	t.Helper()
	h := newHarness(t, common.DeletionDelete, func(s *dbv1alpha1.ClusterDatabaseServer) {
		s.Spec.Host, s.Spec.Port = "", 0
		s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderAzure, Azure: &dbv1alpha1.ClusterAzureDatabase{
			AzureClusterBlock: cloud.AzureClusterBlock{
				AzureBlock:       cloud.AzureBlock{TenantID: azTenant, SubscriptionID: azSub, ResourceGroup: azGroup},
				EndpointOverride: f.srv.URL},
			AzureTarget: dbv1alpha1.AzureTarget{ServerName: "orders"},
		}}
		s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret,
			SecretRef: &cloud.SecretRef{Name: "azure", Namespace: s.Spec.AdminCredentialsSecretRef.Namespace}}
	})
	h.must(env.Client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: h.ops},
		Data: map[string][]byte{cloud.KeyAzureClientID: []byte("11111111-1111-4111-8111-111111111111"),
			cloud.KeyAzureClientSecret: []byte("entra-client-secret")},
	}))
	h.cfg.Egress, h.cfg.EgressMode, h.cfg.CloudRootCAs = egress.Policy{}, egress.ModeAllowPrivate, f.roots
	// No retries: a 5xx of the fake is answered once, not after the SDK's backoff.
	h.cfg.ARM = func(c *cloud.AzureClient, sub string) (ARMAPI, error) {
		c.ClientOptions.Retry = policy.RetryOptions{MaxRetries: -1}
		return NewARM(c, sub)
	}
	h.get(h.server)
	h.server.Status.Discovered = map[string]string{"endpoint": azFQDN, "port": "5432", "region": "West Europe"}
	h.must(env.Client.Status().Update(context.Background(), h.server))
	return h
}

func TestAzureCloudOf(t *testing.T) {
	s := &dbv1alpha1.ClusterDatabaseServer{}
	s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderAzure, Azure: &dbv1alpha1.ClusterAzureDatabase{
		AzureClusterBlock: cloud.AzureClusterBlock{AzureBlock: cloud.AzureBlock{TenantID: azTenant, SubscriptionID: azSub, ResourceGroup: azGroup},
			EndpointOverride: "https://x"},
		AzureTarget: dbv1alpha1.AzureTarget{ServerName: "orders"}}}
	s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	cs := cloudOf(s)
	if cs.kind != cloud.ProviderAzure || cs.scope != cloud.ScopeCluster || cs.azure.EndpointOverride != "https://x" ||
		cs.azureTarget.ServerName != "orders" || cs.creds == nil || providerOf(cs) != cloud.ProviderAzure {
		t.Errorf("cluster = %+v", cs)
	}
	n := &dbv1alpha1.DatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "team"}}
	n.Spec.Provider = &dbv1alpha1.Provider{Type: cloud.ProviderAzure, Azure: &dbv1alpha1.AzureDatabase{
		AzureBlock:  cloud.AzureBlock{TenantID: azTenant, SubscriptionID: azSub, ResourceGroup: azGroup},
		AzureTarget: dbv1alpha1.AzureTarget{ServerName: "orders"}}}
	n.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "k"}}
	cs = cloudOf(n)
	if cs.kind != cloud.ProviderAzure || cs.scope != cloud.ScopeNamespaced || cs.ns != "team" || cs.azure.EndpointOverride != "" ||
		cs.azure.SubscriptionID != azSub || cs.creds.SecretRef.Name != "k" {
		t.Errorf("namespaced = %+v", cs)
	}
	// A provider whose block is missing has no spec.
	if cloudOf(&dbv1alpha1.ClusterDatabaseServer{Spec: dbv1alpha1.ClusterDatabaseServerSpec{
		Provider: &dbv1alpha1.ClusterProvider{Type: cloud.ProviderAzure}}}) != nil {
		t.Error("a provider without a block has a cloud spec")
	}
}

// stubCredential is a token credential that is never asked for a token.
type stubCredential struct{}

func (stubCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errors.New("not used")
}

func TestNewARM(t *testing.T) {
	public := azcore.ClientOptions{Cloud: azcloud.AzurePublic}
	// Without an endpointOverride the client has the public cloud's endpoint.
	if api, err := NewARM(&cloud.AzureClient{Credential: stubCredential{}, ClientOptions: public}, azSub); err != nil || api == nil {
		t.Errorf("default endpoint: %v, %v", api, err)
	}
	// With one, a copy of the cloud's services names it, and the shared map is
	// left alone.
	if api, err := NewARM(&cloud.AzureClient{Credential: stubCredential{}, ClientOptions: public, Endpoint: "https://arm.test"}, azSub); err != nil || api == nil {
		t.Errorf("override: %v, %v", api, err)
	}
	if azcloud.AzurePublic.Services[azcloud.ResourceManager].Endpoint == "https://arm.test" {
		t.Error("the public cloud's services were changed")
	}
	// A cloud with no Resource Manager is an error, not a panic.
	bad := azcore.ClientOptions{Cloud: azcloud.Configuration{ActiveDirectoryAuthorityHost: "https://login.test"}}
	if _, err := NewARM(&cloud.AzureClient{Credential: stubCredential{}, ClientOptions: bad}, azSub); err == nil {
		t.Error("a cloud without Resource Manager was accepted")
	}
}

func TestDescribeAzure(t *testing.T) {
	ctx := context.Background()
	f := newFakeARM(t)
	h := azureHarness(t, f)
	cs := cloudOf(h.server)

	d, err := h.cfg.discover(ctx, cs, server.Connection{})
	if err != nil {
		t.Fatal(err)
	}
	if d["endpoint"] != azFQDN || d["port"] != "5432" || d["region"] != "West Europe" || d["resourceId"] != armServerPath || len(d) != 4 {
		t.Errorf("discovered = %v", d)
	}
	// The API saw the token the authority issued, on the server's own path.
	if r := f.requests(); len(r) != 1 || r[0] != "GET "+armServerPath {
		t.Errorf("requests = %v", r)
	}
	f.mu.Lock()
	authz := append([]string(nil), f.authz...)
	f.mu.Unlock()
	if len(authz) != 1 || authz[0] != "Bearer fake-entra-token" {
		t.Errorf("Authorization = %v", authz)
	}

	// A server that is not usable yet, or whose answer is not what is expected.
	for name, tc := range map[string]struct {
		body, reason string
		transient    bool
	}{
		"starting":               {serverWith(`"state": "Ready"`, `"state": "Starting"`), ReasonCloudError, true},
		"updating":               {serverWith(`"state": "Ready"`, `"state": "Updating"`), ReasonCloudError, true},
		"stopped":                {serverWith(`"state": "Ready"`, `"state": "Stopped"`), ReasonCloudError, false},
		"stopping":               {serverWith(`"state": "Ready"`, `"state": "Stopping"`), ReasonCloudError, false},
		"disabled":               {serverWith(`"state": "Ready"`, `"state": "Disabled"`), ReasonCloudError, false},
		"dropping":               {serverWith(`"state": "Ready"`, `"state": "Dropping"`), ReasonCloudError, false},
		"starting, no host name": {`{"properties":{"state":"Starting"}}`, ReasonCloudError, true},
		"no host name":           {serverWith(`"fullyQualifiedDomainName": "`+azFQDN+`"`, `"fullyQualifiedDomainName": ""`), ReasonCloudError, false},
		"no properties":          {`{"id":"x"}`, ReasonCloudError, false},
		"address as name":        {serverWith(azFQDN, "10.0.0.5"), ReasonCloudError, false},
		"bad name":               {serverWith(azFQDN, "a_b.example"), ReasonCloudError, false},
	} {
		f.server = tc.body
		_, err := h.cfg.discover(ctx, cs, server.Connection{})
		if reasonOf(t, err) != tc.reason || isTransient(err) != tc.transient {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The state and the optional fields may be absent.
	f.server = `{"properties":{"fullyQualifiedDomainName":"` + azFQDN + `"}}`
	if d, err = h.cfg.discover(ctx, cs, server.Connection{}); err != nil || d["endpoint"] != azFQDN || d["region"] != "" || d["resourceId"] != "" {
		t.Errorf("bare answer: %v, %v", d, err)
	}
	f.server = recordedAzureServer

	// A server that does not exist.
	missing := *cs
	missing.azureTarget.ServerName = "absent"
	if _, err = h.cfg.discover(ctx, &missing, server.Connection{}); reasonOf(t, err) != ReasonTargetNotFound {
		t.Errorf("missing server: %v", err)
	}
}

func TestDescribeAzureFailuresAreCategories(t *testing.T) {
	ctx := context.Background()
	f := newFakeARM(t)
	h := azureHarness(t, f)
	cs := cloudOf(h.server)
	for code, tc := range map[int]struct {
		reason    string
		transient bool
	}{
		http.StatusNotFound:        {ReasonTargetNotFound, false},
		http.StatusForbidden:       {ReasonCloudError, false},
		http.StatusBadRequest:      {ReasonCloudError, false},
		http.StatusTooManyRequests: {ReasonCloudError, true},
		http.StatusRequestTimeout:  {ReasonCloudError, true},
		http.StatusBadGateway:      {ReasonCloudError, true},
	} {
		f.status = code
		_, err := h.cfg.discover(ctx, cs, server.Connection{})
		var ce *CloudError
		if !errors.As(err, &ce) || ce.Reason != tc.reason || ce.transient != tc.transient {
			t.Errorf("%d: %v", code, err)
			continue
		}
		if strings.Contains(err.Error(), azSecretText) || strings.Contains(err.Error(), azSub) || strings.Contains(err.Error(), azGroup) {
			t.Errorf("%d: the API text leaked: %v", code, err)
		}
	}
	f.status = 0

	// The authority refusing the client secret is a category of its own, and
	// its text does not leak either; an authority that is down is transient.
	f.entra = http.StatusUnauthorized
	_, err := h.cfg.discover(ctx, cs, server.Connection{})
	if reasonOf(t, err) != ReasonCredentialsUnavailable || isTransient(err) || strings.Contains(err.Error(), azSecretText) {
		t.Errorf("refused secret: %v", err)
	}
	f.entra = http.StatusServiceUnavailable
	if _, err = h.cfg.discover(ctx, cs, server.Connection{}); !isTransient(err) {
		t.Errorf("authority down: %v", err)
	}
	f.entra = 0

	// Errors that are not API answers.
	if e := armErr(context.DeadlineExceeded); !isTransient(e) {
		t.Error("a deadline is not transient")
	}
	if e := armErr(timeoutErr{}); !isTransient(e) {
		t.Error("a network timeout is not transient")
	}
	if e := armErr(errors.New("x")); isTransient(e) || armStatus(errors.New("x")) != 0 || reasonOf(t, e) != ReasonCloudError {
		t.Error("a plain error is transient")
	}
	refused := &azidentity.AuthenticationFailedError{RawResponse: &http.Response{StatusCode: http.StatusBadRequest}}
	if e := armErr(refused); reasonOf(t, e) != ReasonCredentialsUnavailable || isTransient(e) {
		t.Error("a refused credential")
	}
	// An authentication failure with no response says nothing about the
	// credential (the authority was not reached): it is retried. A cancelled
	// context is transient too, and a server error of the authority.
	if e := armErr(&azidentity.AuthenticationFailedError{}); reasonOf(t, e) != ReasonCloudError || !isTransient(e) {
		t.Error("an authentication error without a response")
	}
	if e := armErr(context.Canceled); !isTransient(e) {
		t.Error("a cancelled context is not transient")
	}
	if e := armErr(&azidentity.AuthenticationFailedError{RawResponse: &http.Response{StatusCode: http.StatusBadGateway}}); !isTransient(e) {
		t.Error("an authority server error is not transient")
	}
}

func TestDiscoverAzureClientErrors(t *testing.T) {
	ctx := context.Background()
	f := newFakeARM(t)
	h := azureHarness(t, f)
	cs := cloudOf(h.server)

	// In strict mode an endpointOverride is refused; a namespaced server has
	// none, and cannot use workload identity.
	strict := h.cfg
	strict.EgressMode = egress.ModeStrict
	if _, err := strict.discover(ctx, cs, server.Connection{}); reasonOf(t, err) != ReasonEndpointOverrideForbidden {
		t.Errorf("override in strict mode: %v", err)
	}
	// Credentials: a missing Secret, a Secret without the keys and a client that
	// is not configured are categories.
	missing := *cs
	missing.creds = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "absent", Namespace: h.ops}}
	if _, err := h.cfg.discover(ctx, &missing, server.Connection{}); reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("missing Secret: %v", err)
	}
	h.must(env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "azure-bad", Namespace: h.ops},
		Data: map[string][]byte{cloud.KeyAzureClientID: []byte("x")}}))
	bad := *cs
	bad.creds = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "azure-bad", Namespace: h.ops}}
	if _, err := h.cfg.discover(ctx, &bad, server.Connection{}); reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("Secret without a client secret: %v", err)
	}
	none := h.cfg
	none.ARM = nil
	if _, err := none.discover(ctx, cs, server.Connection{}); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("no client: %v", err)
	}
	broken := h.cfg
	broken.ARM = func(*cloud.AzureClient, string) (ARMAPI, error) { return nil, errors.New(azSecretText) }
	if _, err := broken.discover(ctx, cs, server.Connection{}); reasonOf(t, err) != ReasonCloudError || strings.Contains(err.Error(), azSecretText) {
		t.Errorf("client error: %v", err)
	}
	// A transport that cannot release idle connections is used all the same.
	odd := h.cfg
	odd.ARM = func(c *cloud.AzureClient, sub string) (ARMAPI, error) {
		c.ClientOptions.Transport = stubTransport{}
		return NewARM(c, sub)
	}
	if _, err := odd.discover(ctx, cs, server.Connection{}); err == nil {
		t.Error("the stub transport answered")
	}

	// The API's failures reach the server's condition as categories.
	f.status = http.StatusNotFound
	_, err := h.cfg.discover(ctx, cs, server.Connection{})
	record(h.server, cs, nil, err)
	h.wantCondition(h.server.Status.Conditions, ConditionCloudResolved, metav1.ConditionFalse, ReasonTargetNotFound)
	if h.server.Status.Discovered != nil {
		t.Errorf("discovered = %v", h.server.Status.Discovered)
	}
}

// stubTransport is an HTTP transport that fails every request.
type stubTransport struct{}

func (stubTransport) Do(*http.Request) (*http.Response, error) { return nil, errors.New("stub") }

func TestProberDiscoversTheAzureAddress(t *testing.T) {
	f := newFakeARM(t)
	h := azureHarness(t, f)
	fake := &pgFake{answer: versionAnswer}
	cfg := h.cfg
	cfg.Connect = fake.connect
	srv := &dbv1alpha1.ClusterDatabaseServer{}
	h.get(h.server)
	h.server.DeepCopyInto(srv)
	srv.Status.Discovered = nil
	conn := probeConn()
	conn.ServerName = ""
	v, err := probeOnce(context.Background(), Prober{Config: cfg}, srv, conn)
	if err != nil || v == "" || len(fake.targets) != 1 {
		t.Fatalf("probe: %q, %v, %d connections", v, err, len(fake.targets))
	}
	// The server's host name is the address and, with no tls.serverName, the
	// name its certificate is verified against.
	if tg := fake.targets[0]; tg.Host != azFQDN || tg.Port != 5432 || tg.ServerName != "" {
		t.Errorf("target = %+v", tg)
	}
	if srv.Status.Discovered["resourceId"] != armServerPath {
		t.Errorf("discovered = %v", srv.Status.Discovered)
	}
}

// A Password-mode claim on an Azure server is today's role logic, with the
// Azure cloud key in its binding and no AWS region or Cloud SQL connection name.
func TestAzurePasswordRole(t *testing.T) {
	f := newFakeARM(t)
	h := azureHarness(t, f)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	role := h.role("orders-app", "orders", nil)
	h.reconcileOK(rr, role)
	data := bindingSecret(h, role)
	if data["host"] != azFQDN || data["port"] != "5432" || data["cloud"] != "azure" || data["authMode"] != "Password" ||
		data["password"] == "" || data["uri"] == "" {
		t.Errorf("binding = %v", data)
	}
	for _, k := range []string{"region", "instanceConnectionName", "iamResourceArn"} {
		if _, ok := data[k]; ok {
			t.Errorf("the binding has %s: %v", k, data)
		}
	}
	if len(f.requests()) != 0 {
		t.Errorf("Password mode called Resource Manager: %v", f.requests())
	}
	h.fake.want(t, "CREATE ROLE")

	// IAM mode needs an Entra principal (azure_iam_test.go has its lifecycle).
	h.rec.Reset()
	iam := h.role("orders-iam", "orders", func(r *dbv1alpha1.DatabaseRole) { r.Spec.AuthMode = dbv1alpha1.AuthModeIAM })
	if err := h.reconcile(rr, iam); !errors.Is(err, ErrIAMPrincipalInvalidEntra) {
		t.Errorf("IAM on Azure without a principal: %v", err)
	}
	h.rec.AssertEvent(t, "Warning", ReasonIAMPrincipalRequired, "")
}
