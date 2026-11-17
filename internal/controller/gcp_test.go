package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

const (
	gcpProject   = "proj-12345"
	gcpConnName  = "proj-12345:europe-west1:orders"
	gcpPrincipal = "app@proj-12345.iam"
	// gcpSecretText is in the answers of the fake API and must reach no error,
	// condition or Event.
	gcpSecretText = "sa@proj-12345.iam.gserviceaccount.com"
)

// recordedInstance is the shape of an answer of instances.get for a PostgreSQL
// instance with a public and a private address (fields the operator does not
// read are left out).
const recordedInstance = `{
  "kind": "sql#instance",
  "state": "RUNNABLE",
  "databaseVersion": "POSTGRES_16",
  "settings": {"tier": "db-custom-2-7680", "activationPolicy": "ALWAYS", "ipConfiguration": {"ipv4Enabled": true, "sslMode": "ENCRYPTED_ONLY"}},
  "instanceType": "CLOUD_SQL_INSTANCE",
  "ipAddresses": [
    {"type": "PRIMARY", "ipAddress": "34.72.10.20"},
    {"type": "PRIVATE", "ipAddress": "10.20.0.5"},
    {"type": "OUTGOING", "ipAddress": "34.72.10.99"}
  ],
  "serverCaCert": {"kind": "sql#sslCert", "certSerialNumber": "0", "commonName": "C=US,O=Google\\, Inc,CN=Google Cloud SQL Server CA"},
  "project": "proj-12345",
  "serviceAccountEmailAddress": "p123-abc@gcp-sa-cloud-sql.iam.gserviceaccount.com",
  "backendType": "SECOND_GEN",
  "connectionName": "proj-12345:europe-west1:orders",
  "name": "orders",
  "region": "europe-west1",
  "gceZone": "europe-west1-b",
  "dnsName": "ab12cd34ef56.xyz789.europe-west1.sql.goog."
}`

// instanceWith is recordedInstance with fields changed.
func instanceWith(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(recordedInstance), &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeSQL is the Cloud SQL Admin API and the Google OAuth token endpoint, as one
// HTTP server. The operator reaches it the way it reaches the real ones:
// through cloud.Egress.GCP, with a service account key (the key's token_uri is
// the server) and endpointOverride (the Policy of the tests blocks nothing, so
// loopback stands for a private address).
type fakeSQL struct {
	srv      *httptest.Server
	mu       sync.Mutex
	reqs     []string
	bodies   []string
	authz    []string
	instance string
	// fail forces an HTTP status for a route: "GET instance", "POST users",
	// "DELETE users" or "GET operation".
	fail map[string]int
	// pending is the number of polls of an operation that say it is running.
	pending int
	// insertDone makes users.insert and users.delete answer DONE at once.
	insertDone bool
	// opErr gives a finished operation an error.
	opErr bool
}

func newFakeSQL(t *testing.T) *fakeSQL {
	t.Helper()
	f := &fakeSQL{instance: recordedInstance, fail: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSQL) operation(status string) string {
	e := ""
	if f.opErr && status == "DONE" {
		e = `,"error":{"kind":"sql#operationErrors","errors":[{"kind":"sql#operationError","code":"ERROR","message":"` + gcpSecretText + `"}]}`
	}
	return `{"kind":"sql#operation","name":"op-1","status":"` + status + `","operationType":"CREATE_USER"` + e + `}`
}

func (f *fakeSQL) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/token") {
		_, _ = io.WriteString(w, `{"access_token":"fake-google-token","token_type":"Bearer","expires_in":3600}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	route := ""
	switch {
	case strings.Contains(r.URL.Path, "/operations/"):
		route = "GET operation"
	case strings.HasSuffix(r.URL.Path, "/users"):
		route = r.Method + " users"
	case strings.Contains(r.URL.Path, "/instances/"):
		route = "GET instance"
	}
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	f.bodies = append(f.bodies, string(body))
	f.authz = append(f.authz, r.Header.Get("Authorization"))
	if code := f.fail[route]; code != 0 {
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"denied for %s","errors":[{"reason":"forbidden"}]}}`, code, gcpSecretText)
		return
	}
	switch route {
	case "GET instance":
		_, _ = io.WriteString(w, f.instance)
	case "POST users", "DELETE users":
		status := "PENDING"
		if f.insertDone {
			status = "DONE"
		}
		_, _ = io.WriteString(w, f.operation(status))
	case "GET operation":
		status := "DONE"
		if f.pending > 0 {
			f.pending--
			status = "RUNNING"
		}
		_, _ = io.WriteString(w, f.operation(status))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
	}
}

func (f *fakeSQL) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *fakeSQL) count(prefix string) int {
	n := 0
	for _, r := range f.requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// body is the body of the last request that starts with prefix.
func (f *fakeSQL) body(prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if strings.HasPrefix(f.reqs[i], prefix) {
			return f.bodies[i]
		}
	}
	return ""
}

var (
	gcpKeyOnce sync.Once
	gcpKeyPEM  string
)

// serviceAccountKey is a service_account key with a generated private key
// whose token_uri is tokenURI.
func serviceAccountKey(t *testing.T, tokenURI string) []byte {
	t.Helper()
	gcpKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		gcpKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	})
	b, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": gcpProject, "private_key_id": "kid", "private_key": gcpKeyPEM,
		"client_email": gcpSecretText, "client_id": "1", "token_uri": tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fastPoll makes the operation polling quick, and restores it after the test.
func fastPoll(t *testing.T) {
	t.Helper()
	poll, wait := sqlAdminPoll, sqlAdminWait
	sqlAdminPoll, sqlAdminWait = time.Millisecond, 5*time.Second
	t.Cleanup(func() { sqlAdminPoll, sqlAdminWait = poll, wait })
}

// gcpHarness is a harness whose server is a Cloud SQL one that the probe has
// resolved, with the fake as its Admin API and token endpoint.
func gcpHarness(t *testing.T, f *fakeSQL) *harness {
	t.Helper()
	fastPoll(t)
	h := newHarness(t, common.DeletionDelete, func(s *dbv1alpha1.ClusterDatabaseServer) {
		s.Spec.Host, s.Spec.Port = "", 0
		s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderGCP, GCP: &dbv1alpha1.ClusterGCPDatabase{
			GCPClusterBlock: cloud.GCPClusterBlock{
				GCPBlock: cloud.GCPBlock{Project: gcpProject, Location: "europe-west1"}, EndpointOverride: f.srv.URL},
			GCPTarget: dbv1alpha1.GCPTarget{Instance: "orders"},
		}}
		s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret,
			SecretRef: &cloud.SecretRef{Name: "gcp", Namespace: s.Spec.AdminCredentialsSecretRef.Namespace}}
	})
	h.must(env.Client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp", Namespace: h.ops},
		Data:       map[string][]byte{cloud.KeyGCPCredentials: serviceAccountKey(t, f.srv.URL+"/token")},
	}))
	h.cfg.Egress, h.cfg.EgressMode, h.cfg.SQLAdmin = egress.Policy{}, egress.ModeAllowPrivate, NewSQLAdmin
	h.get(h.server)
	h.server.Status.Discovered = map[string]string{
		"endpoint": "10.20.0.5", "port": "5432", "region": "europe-west1",
		dbv1alpha1.DiscoveredInstanceConnectionName: gcpConnName, dbv1alpha1.DiscoveredDNSName: "ab12cd34ef56.xyz789.europe-west1.sql.goog",
	}
	h.must(env.Client.Status().Update(context.Background(), h.server))
	return h
}

func gcpIAMClaim(principal string) func(*dbv1alpha1.DatabaseRole) {
	return func(r *dbv1alpha1.DatabaseRole) {
		r.Spec.AuthMode = dbv1alpha1.AuthModeIAM
		if principal != "" {
			r.Spec.IAM = &dbv1alpha1.IAMSpec{Principal: principal}
		}
	}
}

func TestGCPCloudOf(t *testing.T) {
	s := &dbv1alpha1.ClusterDatabaseServer{}
	s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderGCP, GCP: &dbv1alpha1.ClusterGCPDatabase{
		GCPClusterBlock: cloud.GCPClusterBlock{GCPBlock: cloud.GCPBlock{Project: gcpProject, Location: "europe-west1"}, EndpointOverride: "http://x"},
		GCPTarget:       dbv1alpha1.GCPTarget{Instance: "orders"}}}
	s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	cs := cloudOf(s)
	if cs.kind != cloud.ProviderGCP || cs.scope != cloud.ScopeCluster || cs.gcp.EndpointOverride != "http://x" || cs.connectionName() != gcpConnName ||
		cs.creds == nil || providerOf(cs) != cloud.ProviderGCP {
		t.Errorf("cluster = %+v", cs)
	}
	n := &dbv1alpha1.DatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "sql", Namespace: "team"}}
	n.Spec.Provider = &dbv1alpha1.Provider{Type: cloud.ProviderGCP, GCP: &dbv1alpha1.GCPDatabase{
		GCPBlock: cloud.GCPBlock{Project: gcpProject, Location: "europe-west1"}, GCPTarget: dbv1alpha1.GCPTarget{Instance: "orders"}}}
	n.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "k"}}
	cs = cloudOf(n)
	if cs.kind != cloud.ProviderGCP || cs.scope != cloud.ScopeNamespaced || cs.ns != "team" || cs.gcp.EndpointOverride != "" || cs.creds.SecretRef.Name != "k" {
		t.Errorf("namespaced = %+v", cs)
	}
	if providerOf(nil) != "" || providerOf(cloudOf(awsServer("eu-west-1"))) != cloud.ProviderAWS {
		t.Error("providerOf")
	}
	// A provider whose block is missing has no spec.
	if cloudOf(&dbv1alpha1.ClusterDatabaseServer{Spec: dbv1alpha1.ClusterDatabaseServerSpec{
		Provider: &dbv1alpha1.ClusterProvider{Type: cloud.ProviderGCP}}}) != nil {
		t.Error("a provider without a block has a cloud spec")
	}
}

func newAPI(t *testing.T, f *fakeSQL) SQLAdminAPI {
	t.Helper()
	api, err := NewSQLAdmin(context.Background(), &cloud.GCPClient{HTTPClient: f.srv.Client(), Endpoint: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func TestNewSQLAdmin(t *testing.T) {
	// Without an endpointOverride the client has the Google default endpoint.
	if api, err := NewSQLAdmin(context.Background(), &cloud.GCPClient{HTTPClient: http.DefaultClient}); err != nil || api == nil {
		t.Errorf("default endpoint: %v, %v", api, err)
	}
	// An option the SDK refuses is an error, not a panic.
	_, err := newSQLAdmin(context.Background(), &cloud.GCPClient{HTTPClient: http.DefaultClient},
		option.WithAPIKey("k"), option.WithoutAuthentication())
	if err == nil {
		t.Error("conflicting options were accepted")
	}
}

func TestDescribeCloudSQL(t *testing.T) {
	ctx := context.Background()
	f := newFakeSQL(t)
	api := newAPI(t, f)
	h := gcpHarness(t, f)
	cs := cloudOf(h.server)

	// Private when the operator may dial private addresses, public otherwise.
	d, err := describeCloudSQL(ctx, api, cs, true)
	want := map[string]string{"endpoint": "10.20.0.5", "port": "5432", "region": "europe-west1",
		"instanceConnectionName": gcpConnName, "dnsName": "ab12cd34ef56.xyz789.europe-west1.sql.goog"}
	if err != nil || len(d) != len(want) {
		t.Fatalf("private: %v, %v", d, err)
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("%s = %q, want %q", k, d[k], v)
		}
	}
	if d, err = describeCloudSQL(ctx, api, cs, false); err != nil || d["endpoint"] != "34.72.10.20" {
		t.Errorf("public: %v, %v", d, err)
	}
	if got := f.requests()[0]; !strings.HasPrefix(got, "GET /v1/projects/proj-12345/instances/orders?") {
		t.Errorf("request = %q", got)
	}

	// Only a private address: it is used even when private is not preferred.
	f.instance = instanceWith(t, func(m map[string]any) {
		m["ipAddresses"] = []any{map[string]any{"type": "PRIVATE", "ipAddress": "10.20.0.5"}}
		delete(m, "region")
		m["dnsName"] = "not a host/name"
	})
	if d, err = describeCloudSQL(ctx, api, cs, false); err != nil || d["endpoint"] != "10.20.0.5" || d["region"] != "" || d["dnsName"] != "" {
		t.Errorf("private only: %v, %v", d, err)
	}

	// An IPv4-mapped address is used as the IPv4 address.
	f.instance = instanceWith(t, func(m map[string]any) {
		m["ipAddresses"] = []any{map[string]any{"type": "PRIMARY", "ipAddress": "::ffff:203.0.113.5"}}
	})
	if d, err = describeCloudSQL(ctx, api, cs, false); err != nil || d["endpoint"] != "203.0.113.5" {
		t.Errorf("mapped: %v, %v", d, err)
	}

	// An address that is not a routable unicast one is never used.
	for _, ip := range []string{"127.0.0.1", "0.0.0.0", "169.254.169.254", "224.0.0.1", "::ffff:127.0.0.1", "not an ip", ""} {
		f.instance = instanceWith(t, func(m map[string]any) {
			m["ipAddresses"] = []any{map[string]any{"type": "PRIMARY", "ipAddress": ip}}
		})
		if _, err = describeCloudSQL(ctx, api, cs, true); reasonOf(t, err) != ReasonCloudError {
			t.Errorf("address %q: %v", ip, err)
		}
	}
	f.instance = instanceWith(t, func(m map[string]any) { delete(m, "ipAddresses") })
	if _, err = describeCloudSQL(ctx, api, cs, true); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("no addresses: %v", err)
	}

	// Not PostgreSQL, or not in the named location.
	f.instance = instanceWith(t, func(m map[string]any) { m["databaseVersion"] = "MYSQL_8_0" })
	if _, err = describeCloudSQL(ctx, api, cs, true); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("MySQL: %v", err)
	}
	f.instance = instanceWith(t, func(m map[string]any) { m["connectionName"] = "proj-12345:us-central1:orders" })
	if _, err = describeCloudSQL(ctx, api, cs, true); reasonOf(t, err) != ReasonTargetNotFound {
		t.Errorf("another location: %v", err)
	}
}

func TestDescribeCloudSQLFailuresAreCategories(t *testing.T) {
	ctx := context.Background()
	f := newFakeSQL(t)
	api := newAPI(t, f)
	cs := cloudOf(gcpHarness(t, f).server)
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
		f.fail["GET instance"] = code
		_, err := describeCloudSQL(ctx, api, cs, true)
		var ce *CloudError
		if !errors.As(err, &ce) || ce.Reason != tc.reason || ce.transient != tc.transient {
			t.Errorf("%d: %v", code, err)
		}
		if strings.Contains(err.Error(), "sa@") || strings.Contains(err.Error(), "denied") {
			t.Errorf("%d: the API text leaked: %v", code, err)
		}
	}
	// A timeout and a cancelled context are transient too, and a plain error is not.
	if e := sqlAdminErr(context.DeadlineExceeded); !isTransient(e) {
		t.Error("a deadline is not transient")
	}
	if e := sqlAdminErr(timeoutErr{}); !isTransient(e) {
		t.Error("a network timeout is not transient")
	}
	if e := sqlAdminErr(context.Canceled); !isTransient(e) {
		t.Error("a cancelled context is not transient")
	}
	if e := sqlAdminErr(errors.New("x")); isTransient(e) || httpStatus(errors.New("x")) != 0 {
		t.Error("a plain error is transient")
	}
}

func isTransient(err error) bool {
	var ce *CloudError
	return errors.As(err, &ce) && ce.transient
}

func TestDiscoverGCP(t *testing.T) {
	ctx := context.Background()
	f := newFakeSQL(t)
	h := gcpHarness(t, f)
	cs := cloudOf(h.server)

	d, err := h.cfg.discover(ctx, cs, server.Connection{})
	if err != nil || d["endpoint"] != "10.20.0.5" || d["instanceConnectionName"] != gcpConnName {
		t.Fatalf("discover: %v, %v", d, err)
	}
	// The API saw the token the key's token endpoint issued.
	f.mu.Lock()
	authz := append([]string(nil), f.authz...)
	f.mu.Unlock()
	if len(authz) == 0 || authz[0] != "Bearer fake-google-token" {
		t.Errorf("Authorization = %v", authz)
	}
	// In strict mode an endpointOverride is refused.
	strict := h.cfg
	strict.EgressMode = egress.ModeStrict
	if _, err = strict.discover(ctx, cs, server.Connection{}); reasonOf(t, err) != ReasonEndpointOverrideForbidden {
		t.Errorf("override in strict mode: %v", err)
	}

	// Credentials: a missing Secret, a key of another type and a client that is
	// not configured are categories.
	missing := *cs
	missing.creds = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "absent", Namespace: h.ops}}
	if _, err = h.cfg.discover(ctx, &missing, server.Connection{}); reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("missing Secret: %v", err)
	}
	h.must(env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gcp-bad", Namespace: h.ops},
		Data: map[string][]byte{cloud.KeyGCPCredentials: []byte(`{"type":"external_account"}`)}}))
	bad := *cs
	bad.creds = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "gcp-bad", Namespace: h.ops}}
	if _, err = h.cfg.discover(ctx, &bad, server.Connection{}); reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("external_account key: %v", err)
	}
	none := h.cfg
	none.SQLAdmin = nil
	if _, err = none.discover(ctx, cs, server.Connection{}); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("no client: %v", err)
	}
	broken := h.cfg
	broken.SQLAdmin = func(context.Context, *cloud.GCPClient) (SQLAdminAPI, error) { return nil, errors.New("sa@x") }
	if _, err = broken.discover(ctx, cs, server.Connection{}); reasonOf(t, err) != ReasonCloudError || strings.Contains(err.Error(), "sa@x") {
		t.Errorf("client error: %v", err)
	}

	// The API's failures reach the server's condition as categories.
	f.fail["GET instance"] = http.StatusNotFound
	_, err = h.cfg.discover(ctx, cs, server.Connection{})
	record(h.server, cs, nil, err)
	c := h.server.Status.Conditions
	h.wantCondition(c, ConditionCloudResolved, metav1.ConditionFalse, ReasonTargetNotFound)
	if h.server.Status.Discovered != nil {
		t.Errorf("discovered = %v", h.server.Status.Discovered)
	}
}

func TestProberDiscoversTheCloudSQLAddress(t *testing.T) {
	f := newFakeSQL(t)
	h := gcpHarness(t, f)
	fake := &pgFake{answer: versionAnswer}
	cfg := h.cfg
	cfg.Connect = fake.connect
	srv := &dbv1alpha1.ClusterDatabaseServer{}
	h.get(h.server)
	h.server.DeepCopyInto(srv)
	srv.Status.Discovered = nil
	conn := probeConn()
	conn.ServerName = ""
	v, err := Prober{Config: cfg}.Probe(context.Background(), srv, conn)
	if err != nil || v == "" || len(fake.targets) != 1 {
		t.Fatalf("probe: %q, %v, %d connections", v, err, len(fake.targets))
	}
	// The instance's address, and the DNS name its certificate carries as the
	// TLS server name (an IP address is not in the certificate).
	if tg := fake.targets[0]; tg.Host != "10.20.0.5" || tg.Port != 5432 || tg.ServerName != "ab12cd34ef56.xyz789.europe-west1.sql.goog" {
		t.Errorf("target = %+v", tg)
	}
	if srv.Status.Discovered["instanceConnectionName"] != gcpConnName {
		t.Errorf("discovered = %v", srv.Status.Discovered)
	}
}

// The TLS server name: an explicit one wins, and so does a host in the spec.
func TestAdminTargetServerName(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no") }
	conn := server.Connection{Admin: map[string][]byte{AdminUsernameKey: []byte("u"), AdminPasswordKey: []byte("p")}}
	disc := map[string]string{"endpoint": "10.0.0.1", "dnsName": "x.sql.goog"}
	for name, tc := range map[string]struct {
		host, serverName, want string
	}{
		"discovered DNS name": {"", "", "x.sql.goog"},
		"explicit name wins":  {"", "pg.internal", "pg.internal"},
		"spec host wins":      {"10.9.9.9", "", ""},
	} {
		c := conn
		c.ServerName = tc.serverName
		tg, err := adminTarget(dbv1alpha1.PostgreSQL{Host: tc.host}, disc, c, dial)
		if err != nil || tg.ServerName != tc.want {
			t.Errorf("%s: %q, %v", name, tg.ServerName, err)
		}
	}
}

func TestWaitOperation(t *testing.T) {
	ctx := context.Background()
	fastPoll(t)
	f := newFakeSQL(t)
	api := newAPI(t, f)
	cs := cloudOf(gcpHarness(t, f).server)

	// A running operation is followed until it is done.
	f.pending = 2
	if err := insertIAMUser(ctx, api, cs, gcpPrincipal); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n := f.count("GET /v1/projects/proj-12345/operations/op-1"); n != 3 {
		t.Errorf("polled %d times", n)
	}
	if body := f.body("POST "); !strings.Contains(body, `"name":"app@proj-12345.iam"`) || !strings.Contains(body, `"type":"CLOUD_IAM_SERVICE_ACCOUNT"`) {
		t.Errorf("insert body = %s", body)
	}
	// One that is done at once is not polled; one that failed is a category.
	f.insertDone = true
	before := f.count("GET /v1/projects/proj-12345/operations/")
	if err := insertIAMUser(ctx, api, cs, "alice@example.com"); err != nil || f.count("GET /v1/projects/proj-12345/operations/") != before {
		t.Errorf("done at once: %v", err)
	}
	if body := f.body("POST "); !strings.Contains(body, `"type":"CLOUD_IAM_USER"`) {
		t.Errorf("insert body = %s", body)
	}
	f.opErr = true
	err := insertIAMUser(ctx, api, cs, gcpPrincipal)
	if reasonOf(t, err) != ReasonCloudError || strings.Contains(err.Error(), "sa@") {
		t.Errorf("failed operation: %v", err)
	}
	f.opErr, f.insertDone = false, false

	// A poll that fails, and one that never ends.
	f.fail["GET operation"] = http.StatusInternalServerError
	if err = insertIAMUser(ctx, api, cs, gcpPrincipal); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("poll failure: %v", err)
	}
	delete(f.fail, "GET operation")
	sqlAdminPoll, sqlAdminWait = time.Second, 10*time.Millisecond
	if err = insertIAMUser(ctx, api, cs, gcpPrincipal); reasonOf(t, err) != ReasonCloudError || !strings.Contains(err.Error(), "in time") {
		t.Errorf("timeout: %v", err)
	}

	// The API refuses the insert, or the delete.
	f.fail["POST users"] = http.StatusForbidden
	if err = insertIAMUser(ctx, api, cs, gcpPrincipal); reasonOf(t, err) != ReasonCloudError || strings.Contains(err.Error(), "denied") {
		t.Errorf("insert refused: %v", err)
	}
	f.fail["DELETE users"] = http.StatusForbidden
	if err = deleteIAMUser(ctx, api, cs, gcpPrincipal); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("delete refused: %v", err)
	}
	// A user that is gone already is deleted.
	f.fail["DELETE users"] = http.StatusNotFound
	if err = deleteIAMUser(ctx, api, cs, gcpPrincipal); err != nil {
		t.Errorf("delete of a gone user: %v", err)
	}
	delete(f.fail, "DELETE users")
	// A delete that runs is pending, not waited for; so is a busy instance.
	f.insertDone = false
	before = f.count("GET /v1/projects/proj-12345/operations/")
	for name, prep := range map[string]func(){
		"running": func() {}, "busy": func() { f.fail["DELETE users"] = http.StatusConflict },
	} {
		prep()
		var pe *lifecycle.PendingError
		if err = deleteIAMUser(ctx, api, cs, gcpPrincipal); !errors.As(err, &pe) || pe.Reason != conditions.Deleting ||
			pe.After != sqlAdminRequeue || strings.Contains(err.Error(), gcpPrincipal) {
			t.Errorf("%s: %v", name, err)
		}
		delete(f.fail, "DELETE users")
	}
	if f.count("GET /v1/projects/proj-12345/operations/") != before {
		t.Error("the delete operation was polled")
	}
	// One that finished with an error is a category.
	f.insertDone, f.opErr = true, true
	if err = deleteIAMUser(ctx, api, cs, gcpPrincipal); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("failed delete: %v", err)
	}
	f.opErr = false
	sqlAdminPoll, sqlAdminWait = time.Millisecond, 5*time.Second
	f.pending, f.insertDone = 0, true
	if err = deleteIAMUser(ctx, api, cs, "app@proj-12345.iam"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if got := f.requests()[len(f.requests())-1]; !strings.HasPrefix(got, "DELETE /v1/projects/proj-12345/instances/orders/users?") ||
		!strings.Contains(got, "name=app%40proj-12345.iam") {
		t.Errorf("delete request = %q", got)
	}
}

func TestValidPrincipal(t *testing.T) {
	for p, want := range map[string]bool{
		"app@proj-12345.iam":                     true,
		"alice@example.com":                      true,
		"a.b+c_d%e@x.y.z":                        true,
		"App@proj-12345.iam":                     false,
		"app@proj-12345.iam.gserviceaccount.com": false,
		"app":                                    false,
		"@x.iam":                                 false,
		"app@":                                   false,
		"app@x.iam;DROP":                         false,
		"app @x.iam":                             false,
		"":                                       false,
		strings.Repeat("a", 60) + "@b.cd":        false,
		"a@" + strings.Repeat("b", 62):           false,
	} {
		if got := ValidPrincipal(p); got != want {
			t.Errorf("ValidPrincipal(%q) = %v, want %v", p, got, want)
		}
	}
	if principalType("app@proj-12345.iam") != userTypeIAMServiceAccount || principalType("alice@example.com") != userTypeIAMUser {
		t.Error("principalType")
	}
}

// An IAM-mode claim on Cloud SQL is an IAM database user the Admin API creates,
// named by the principal, with the marker, the grants and no password; its
// binding names the instance and no password; nothing rotates; deleting
// reassigns the objects and deletes the user.
func TestGCPIAMRoleLifecycle(t *testing.T) {
	f := newFakeSQL(t)
	h := gcpHarness(t, f)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	dbName := pgclient.DatabaseName(h.ns, "orders")
	role := h.role("orders-iam", "orders", gcpIAMClaim(gcpPrincipal))
	q := `"` + gcpPrincipal + `"`

	h.fake.reset()
	f.pending = 1
	h.reconcileOK(rr, role)
	h.get(role)
	claim := h.ns + "/orders-iam/" + string(role.UID)
	if n := f.count("POST /v1/projects/proj-12345/instances/orders/users"); n != 1 {
		t.Errorf("users.insert calls: %d (%v)", n, f.requests())
	}
	if body := f.body("POST "); !strings.Contains(body, `"name":"`+gcpPrincipal+`"`) || !strings.Contains(body, "CLOUD_IAM_SERVICE_ACCOUNT") ||
		strings.Contains(body, "password") {
		t.Errorf("insert body = %s", body)
	}
	h.fake.want(t,
		`|COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=`+claim+`'`,
		`|GRANT CONNECT ON DATABASE "`+dbName+`" TO `+q,
		dbName+`|GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+q,
		`|COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=`+claim+` privileges=ReadWrite'`)
	h.fake.wantNot(t, "CREATE ROLE", "PASSWORD", `"rds_iam"`, "NOLOGIN", "SET role")
	if role.Status.ExternalName != gcpPrincipal || role.Status.Rotation != nil {
		t.Errorf("status = %+v", role.Status)
	}

	data := bindingSecret(h, role)
	for k, v := range map[string]string{
		"type": "postgresql", "provider": "craftainer.dev", "cloud": "gcp", "authMode": "IAM", "instanceConnectionName": gcpConnName,
		"host": "10.20.0.5", "port": "5432", "database": dbName, "username": gcpPrincipal, "sslmode": "verify-full",
	} {
		if got, ok := data[k]; !ok || got != v {
			t.Errorf("binding[%s] = %q (present %v), want %q", k, got, ok, v)
		}
	}
	for _, k := range []string{"password", "uri", "region", "iamResourceArn"} {
		if _, ok := data[k]; ok {
			t.Errorf("the binding has %q: %v", k, data)
		}
	}

	// Steady state: nothing is written and the API is not called.
	h.fake.answer = iamRoleAnswer(gcpPrincipal, claim, "ReadWrite")
	h.fake.reset()
	calls := len(f.requests())
	h.reconcileOK(rr, role)
	h.fake.wantNot(t, "CREATE", "ALTER", "COMMENT", "GRANT", "DROP")
	if len(f.requests()) != calls {
		t.Errorf("the API was called: %v", f.requests()[calls:])
	}

	// Drift: the privileges changed; the user is not inserted again.
	h.get(role)
	role.Spec.Privileges = dbv1alpha1.PrivilegesReadOnly
	h.must(env.Client.Update(context.Background(), role))
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, dbName+`|GRANT SELECT ON ALL TABLES IN SCHEMA public TO `+q, `privileges=ReadOnly'`)
	if f.count("POST ") != 1 {
		t.Errorf("the user was inserted again: %v", f.requests())
	}

	// A role of that name that is someone else's is never touched.
	h.fake.answer = iamRoleAnswer(gcpPrincipal, "other/role/uid", "ReadOnly")
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.get(role)
	h.wantCondition(role.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	h.fake.wantNot(t, "ALTER", "GRANT", "DROP", "COMMENT")
	if f.count("POST ")+f.count("DELETE ") != 1 {
		t.Errorf("the API was called for a role that is not ours: %v", f.requests())
	}

	// Deleting hands the objects to the admin role and deletes the user.
	h.fake.answer = iamRoleAnswer(gcpPrincipal, claim, "ReadOnly")
	f.insertDone = true
	h.must(env.Client.Delete(context.Background(), role))
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `pg_terminate_backend(pid)`, `|GRANT `+q+` TO CURRENT_USER`, dbName+`|REASSIGN OWNED BY `+q+` TO CURRENT_USER`,
		dbName+`|DROP OWNED BY `+q)
	h.fake.wantNot(t, "DROP ROLE")
	if f.count("DELETE /v1/projects/proj-12345/instances/orders/users?") != 1 || !strings.Contains(strings.Join(f.requests(), "\n"), "name=app%40proj-12345.iam") {
		t.Errorf("users.delete calls: %v", f.requests())
	}
	if !h.gone(role) {
		t.Error("the claim is still there")
	}
}

// A user that can only be deleted later keeps the claim: the failure is a
// category, and the claim is released when the user is gone.
func TestGCPIAMRoleDeleteFailure(t *testing.T) {
	f := newFakeSQL(t)
	h := gcpHarness(t, f)
	rr := &RoleReconciler{h.cfg}
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))
	role := h.role("orders-iam", "orders", gcpIAMClaim(gcpPrincipal))
	f.insertDone = true
	h.reconcileOK(rr, role)
	h.get(role)
	claim := h.ns + "/orders-iam/" + string(role.UID)
	h.fake.answer = iamRoleAnswer(gcpPrincipal, claim, "ReadWrite")

	f.fail["DELETE users"] = http.StatusForbidden
	h.must(env.Client.Delete(context.Background(), role))
	err := h.reconcile(rr, role)
	if err == nil || reasonOf(t, err) != ReasonCloudError || strings.Contains(err.Error(), "denied") {
		t.Fatalf("delete: %v", err)
	}
	if h.gone(role) {
		t.Fatal("the claim was released with its user still there")
	}
	delete(f.fail, "DELETE users")
	h.reconcileOK(rr, role)
	if !h.gone(role) {
		t.Error("the claim is still there")
	}
}

// A user whose deletion runs keeps the claim without an error: it is requeued
// (Deleting), and released when the API says the user is gone.
func TestGCPIAMRoleDeletePending(t *testing.T) {
	f := newFakeSQL(t)
	h := gcpHarness(t, f)
	rr := &RoleReconciler{h.cfg}
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))
	role := h.role("orders-iam", "orders", gcpIAMClaim(gcpPrincipal))
	f.insertDone = true
	h.reconcileOK(rr, role)
	h.get(role)
	claim := h.ns + "/orders-iam/" + string(role.UID)
	h.fake.answer = iamRoleAnswer(gcpPrincipal, claim, "ReadWrite")

	f.insertDone = false
	h.must(env.Client.Delete(context.Background(), role))
	h.reconcileOK(rr, role)
	if h.gone(role) {
		t.Fatal("the claim was released with its user still being deleted")
	}
	h.get(role)
	h.wantCondition(role.Status.Conditions, conditions.Ready, metav1.ConditionFalse, conditions.Deleting)
	f.fail["DELETE users"] = http.StatusNotFound
	h.reconcileOK(rr, role)
	if !h.gone(role) {
		t.Error("the claim is still there")
	}
}

// Failures of the Admin API are categories, and a failed create is recorded.
func TestGCPIAMRoleCreateFailures(t *testing.T) {
	f := newFakeSQL(t)
	h := gcpHarness(t, f)
	rr := &RoleReconciler{h.cfg}
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))

	f.fail["POST users"] = http.StatusForbidden
	role := h.role("orders-iam", "orders", gcpIAMClaim(gcpPrincipal))
	err := h.reconcile(rr, role)
	if err == nil || reasonOf(t, err) != ReasonCloudError || strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), "sa@") {
		t.Fatalf("insert refused: %v", err)
	}
	h.fake.wantNot(t, "COMMENT ON ROLE")

	// The user exists in the API but the operation failed: no marker either.
	delete(f.fail, "POST users")
	f.insertDone, f.opErr = true, true
	if err = h.reconcile(rr, role); err == nil || reasonOf(t, err) != ReasonCloudError {
		t.Errorf("failed operation: %v", err)
	}
	h.fake.wantNot(t, "COMMENT ON ROLE")

	// A client that cannot be built fails the same way, for the insert.
	f.insertDone, f.opErr = false, false
	h2 := gcpHarness(t, f)
	h2.cfg.SQLAdmin = nil
	h2.reconcileOK(&DatabaseReconciler{h2.cfg}, h2.database("orders", nil))
	role2 := h2.role("orders-iam", "orders", gcpIAMClaim(gcpPrincipal))
	if err = h2.reconcile(&RoleReconciler{h2.cfg}, role2); err == nil || reasonOf(t, err) != ReasonCloudError {
		t.Errorf("no client: %v", err)
	}
}

// iam.principal is required and checked on Cloud SQL, and refused on AWS.
func TestIAMPrincipalChecks(t *testing.T) {
	f := newFakeSQL(t)
	h := gcpHarness(t, f)
	rr := &RoleReconciler{h.cfg}
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))
	for name, principal := range map[string]string{
		"none": "", "upper case": "App@proj-12345.iam", "service account email": "app@proj-12345.iam.gserviceaccount.com",
		"not an email": "app", "too long": strings.Repeat("a", 60) + "@b.cd",
	} {
		h.rec.Reset()
		h.fake.reset()
		role := h.role("iam-"+strings.ReplaceAll(strings.ToLower(name), " ", "-"), "orders", gcpIAMClaim(principal))
		if err := h.reconcile(rr, role); !errors.Is(err, ErrIAMPrincipalInvalid) || !strings.Contains(err.Error(), ReasonIAMPrincipalRequired) {
			t.Errorf("%s: %v", name, err)
		}
		h.rec.AssertEvent(t, "Warning", ReasonIAMPrincipalRequired, "")
		h.fake.wantNot(t, "COMMENT", "GRANT")
		h.get(role)
		if role.Status.ExternalName == principal && principal != "" {
			t.Errorf("%s: the invalid principal became the role name", name)
		}
	}
	if f.count("POST ") != 0 {
		t.Errorf("a user was inserted: %v", f.requests())
	}

	// On AWS the principal is refused.
	aws := cloudHarness(t, common.DeletionRetain)
	aws.reconcileOK(&DatabaseReconciler{aws.cfg}, aws.database("orders", nil))
	aws.fake.reset()
	role := aws.role("orders-iam", "orders", gcpIAMClaim(gcpPrincipal))
	if err := aws.reconcile(&RoleReconciler{aws.cfg}, role); !errors.Is(err, ErrIAMPrincipalRefused) {
		t.Errorf("AWS with a principal: %v", err)
	}
	aws.fake.wantNot(t, "CREATE", "GRANT")
}

// The deletion of a role that is not the claim's leaves it alone, and the
// user of a role that cannot be reached by the API fails with a category.
func TestDeleteCloudSQLUserEdges(t *testing.T) {
	ctx := context.Background()
	h := gcpHarness(t, newFakeSQL(t))
	fake := &pgFake{answer: iamRoleAnswer(gcpPrincipal, "other/claim/uid", "ReadWrite")}
	e := &roleExternal{
		be: &backend{admin: pgclient.Admin{Open: pgclient.Opener(nil, fake.connect, pgclient.Target{})}, provider: cloud.ProviderGCP,
			cs: cloudOf(h.server), cfg: Config{Client: h.cfg.Client, OperatorNamespace: h.ops}},
		base: gcpPrincipal, id: "team/app/uid", database: "db",
	}
	if err := e.deleteCloudSQLUser(ctx); err != nil {
		t.Errorf("a role that is not ours: %v", err)
	}
	fake.mu.Lock()
	skipped := strings.Contains(strings.Join(fake.stmts, "\n"), "REASSIGN")
	fake.mu.Unlock()
	if skipped {
		t.Error("a role that is not ours was dropped")
	}
	// Ours, but the client cannot be built: workload identity is not there.
	fake.answer = iamRoleAnswer(gcpPrincipal, "team/app/uid", "ReadWrite")
	e.be.cs = &cloudSpec{kind: cloud.ProviderGCP, scope: cloud.ScopeCluster, gcp: e.be.cs.gcp, gcpTarget: e.be.cs.gcpTarget,
		creds: &cloud.Credentials{Source: cloud.SourceWorkloadIdentity}}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing.json")
	if err := e.deleteCloudSQLUser(ctx); reasonOf(t, err) == "" {
		t.Errorf("no client: %v", err)
	}
}

// A Password-mode claim on Cloud SQL is today's role logic, with the Cloud SQL
// keys in its binding and no AWS region.
func TestGCPPasswordRole(t *testing.T) {
	f := newFakeSQL(t)
	h := gcpHarness(t, f)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	role := h.role("orders-app", "orders", nil)
	h.reconcileOK(rr, role)
	data := bindingSecret(h, role)
	if data["host"] != "10.20.0.5" || data["port"] != "5432" || data["cloud"] != "gcp" || data["authMode"] != "Password" ||
		data["instanceConnectionName"] != gcpConnName || data["password"] == "" || data["uri"] == "" {
		t.Errorf("binding = %v", data)
	}
	if _, ok := data["region"]; ok {
		t.Errorf("the binding has an AWS region: %v", data)
	}
	if len(f.requests()) != 0 {
		t.Errorf("Password mode called the Admin API: %v", f.requests())
	}
	h.fake.want(t, "CREATE ROLE")
}
