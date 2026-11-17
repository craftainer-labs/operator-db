//go:build integration && cloudsql

package integration

// The Cloud SQL backend (NKdb-2) against a fake of the Cloud SQL Admin API and
// the TLS PostgreSQL of the other tests as the "Cloud SQL" data plane. sqladmin
// has no emulator, so the fake is a small Go test server in this process: it
// answers instances.get with the PostgreSQL's container address, and users.insert
// and users.delete by creating and dropping the role in that PostgreSQL, as
// Cloud SQL does for an IAM database user. The server listens on a private
// address of the host, never loopback, because pkg/egress blocks loopback in
// both modes; EGRESS_MODE=allow-private is in this test's wiring only. Nothing
// validates a Google token or an IAM login here (docs/nfrs/NFR-0005).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/sqladmin/v1"
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
	csOpsNS      = "operator-db-system"
	csProject    = "proj-12345"
	csLocation   = "europe-west1"
	csConnName   = "proj-12345:europe-west1:orders"
	csTLSServer  = "postgres" // a name in the test certificate; the address is a bare IP
	csSecretName = "gcp"
)

// privateAddress is an IPv4 address of this host that is neither loopback nor
// link-local.
func privateAddress(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip := n.IP.To4(); ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip.String()
		}
	}
	t.Fatal("this host has no address that is not loopback")
	return ""
}

// serviceAccountKey is a service_account key with a generated private key.
func serviceAccountKey(t *testing.T, tokenURI string) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": csProject, "private_key_id": "kid",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "operator@" + csProject + ".iam.gserviceaccount.com", "client_id": "1", "token_uri": tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// cloudSQLFake is the Cloud SQL Admin API and the Google token endpoint.
type cloudSQLFake struct {
	t     *testing.T
	pgIP  string
	admin pgclient.Admin
	srv   *httptest.Server
	mu    sync.Mutex
	// inserts and deletes count the users calls; types are the user types seen.
	inserts, deletes int
	types            []string
}

func newCloudSQLFake(t *testing.T, sv server) *cloudSQLFake {
	t.Helper()
	f := &cloudSQLFake{t: t, pgIP: sv.host, admin: sv.admin(func(tg *pgclient.Target) { tg.ServerName = csTLSServer })}
	l, err := net.Listen("tcp", net.JoinHostPort(privateAddress(t), "0"))
	if err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	f.srv.Listener = l
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *cloudSQLFake) exec(ctx context.Context, sql string) error {
	s, err := f.admin.Open(ctx, "")
	if err != nil {
		return err
	}
	defer s.Close(ctx)
	return s.Exec(ctx, sql)
}

func (f *cloudSQLFake) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	done := `{"kind":"sql#operation","name":"op-1","status":"DONE"}`
	switch {
	case strings.HasSuffix(r.URL.Path, "/token"):
		_, _ = fmt.Fprint(w, `{"access_token":"fake-google-token","token_type":"Bearer","expires_in":3600}`)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/instances/"):
		_, _ = fmt.Fprintf(w, `{"kind":"sql#instance","state":"RUNNABLE","databaseVersion":"POSTGRES_18","connectionName":%q,`+
			`"region":%q,"ipAddresses":[{"type":"PRIVATE","ipAddress":%q}]}`, csConnName, csLocation, f.pgIP)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/users"):
		var u sqladmin.User
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil || u.Name == "" {
			http.Error(w, "bad user", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.inserts++
		f.types = append(f.types, u.Type)
		f.mu.Unlock()
		// Cloud SQL creates the PostgreSQL role of an IAM user: it can log in and
		// has no password.
		if err := f.exec(r.Context(), "CREATE ROLE "+pgclient.Ident(u.Name)+" LOGIN"); err != nil {
			http.Error(w, "create role", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, done)
	case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/users"):
		f.mu.Lock()
		f.deletes++
		f.mu.Unlock()
		if err := f.exec(r.Context(), "DROP ROLE IF EXISTS "+pgclient.Ident(r.URL.Query().Get("name"))); err != nil {
			http.Error(w, "drop role", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, done)
	default:
		http.NotFound(w, r)
	}
}

func (f *cloudSQLFake) counts() (inserts, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inserts, f.deletes
}

type cloudSQLHarness struct {
	cfg  controller.Config
	conn libserver.Connection
	pg   server
	f    *cloudSQLFake
	key  map[string][]byte
}

func newCloudSQLHarness(t *testing.T, mode egress.Mode) *cloudSQLHarness {
	t.Helper()
	pg := load(t)
	f := newCloudSQLFake(t, pg)
	key := map[string][]byte{cloud.KeyGCPCredentials: serviceAccountKey(t, f.srv.URL+"/token")}
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: csSecretName, Namespace: csOpsNS}, Data: key}
	h := &cloudSQLHarness{pg: pg, f: f, key: key}
	h.cfg = controller.Config{
		Client: fake.NewClientBuilder().WithObjects(creds).Build(), OperatorNamespace: csOpsNS,
		Egress: egress.NewPolicy(mode), EgressMode: mode, SQLAdmin: controller.NewSQLAdmin, Connect: pgclient.Connect,
	}
	h.conn = libserver.Connection{
		Admin:    map[string][]byte{controller.AdminUsernameKey: []byte(pg.user), controller.AdminPasswordKey: []byte(pg.password)},
		CABundle: pg.ca, ServerName: csTLSServer, MinTLSVersion: 0x0304,
	}
	return h
}

// server is a cluster server for the instance "orders" whose Admin API is the
// fake.
func (h *cloudSQLHarness) server() *dbv1alpha1.ClusterDatabaseServer {
	s := &dbv1alpha1.ClusterDatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "sql-" + suffix(), Generation: 1}}
	s.Spec.AdminDatabase = "postgres"
	s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderGCP, GCP: &dbv1alpha1.ClusterGCPDatabase{
		GCPClusterBlock: cloud.GCPClusterBlock{GCPBlock: cloud.GCPBlock{Project: csProject, Location: csLocation}, EndpointOverride: h.f.srv.URL},
		GCPTarget:       dbv1alpha1.GCPTarget{Instance: "orders"},
	}}
	s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: csSecretName, Namespace: csOpsNS}}
	s.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	return s
}

// api is the Admin API client the operator builds, through cloud.Egress.GCP.
func (h *cloudSQLHarness) api(t *testing.T) controller.SQLAdminAPI {
	t.Helper()
	e := cloud.NewEgress(egress.ModeAllowPrivate, nil)
	gc, err := e.GCP(context.Background(), cloud.ScopeCluster,
		cloud.GCPClusterBlock{GCPBlock: cloud.GCPBlock{Project: csProject, Location: csLocation}, EndpointOverride: h.f.srv.URL},
		&cloud.Credentials{Source: cloud.SourceSecret}, h.key)
	if err != nil {
		t.Fatalf("GCP client: %v", err)
	}
	api, err := controller.NewSQLAdmin(context.Background(), gc)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func TestCloudSQLProbeDiscoversAndConnects(t *testing.T) {
	h := newCloudSQLHarness(t, egress.ModeAllowPrivate)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	srv := h.server()
	version, err := controller.Prober{Config: h.cfg}.Probe(ctx, srv, h.conn)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(version, "18") {
		t.Errorf("server_version = %q", version)
	}
	d := srv.Status.Discovered
	if d[dbv1alpha1.DiscoveredEndpoint] != h.pg.host || d[dbv1alpha1.DiscoveredPort] != "5432" ||
		d[dbv1alpha1.DiscoveredInstanceConnectionName] != csConnName || d[dbv1alpha1.DiscoveredRegion] != csLocation {
		t.Errorf("discovered = %v", d)
	}
	if c := meta.FindStatusCondition(srv.Status.Conditions, controller.ConditionCloudResolved); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("CloudResolved = %+v", c)
	}
}

func TestCloudSQLProbeFailuresAreCategories(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var ce *controller.CloudError

	// endpointOverride is refused in strict mode, before any request is made.
	strict := newCloudSQLHarness(t, egress.ModeStrict)
	_, err := controller.Prober{Config: strict.cfg}.Probe(ctx, strict.server(), strict.conn)
	if !errors.As(err, &ce) || ce.Reason != controller.ReasonEndpointOverrideForbidden {
		t.Errorf("override in strict mode: %v", err)
	}

	// An instance in another location than the spec names.
	h := newCloudSQLHarness(t, egress.ModeAllowPrivate)
	srv := h.server()
	srv.Spec.Provider.GCP.Location = "us-central1"
	_, err = controller.Prober{Config: h.cfg}.Probe(ctx, srv, h.conn)
	if !errors.As(err, &ce) || ce.Reason != controller.ReasonTargetNotFound {
		t.Errorf("another location: %v", err)
	}
}

// An IAM database user: Cloud SQL (the fake) creates the login role, the
// operator marks it and grants what the claim says, and deleting hands the
// objects to the admin role and deletes the user.
func TestCloudSQLIAMRole(t *testing.T) {
	ctx := context.Background()
	h := newCloudSQLHarness(t, egress.ModeAllowPrivate)
	admin := h.f.admin
	api := h.api(t)
	db := newDatabase(t, admin, "it/csql/db-uid")
	const claim = "it/csql/role-uid"
	principal := "it-" + suffix() + "@" + csProject + ".iam"
	t.Cleanup(func() { _ = h.f.exec(ctx, "DROP OWNED BY "+pgclient.Ident(principal)+"; DROP ROLE IF EXISTS "+pgclient.Ident(principal)) })
	params := func(priv string) pgclient.RoleParams {
		return pgclient.RoleParams{
			Base: principal, Database: db, ConnLimit: 3, Privileges: priv,
			Marker: pgclient.Marker{Claim: claim, Privileges: priv},
		}
	}
	insert := func(ctx context.Context) error {
		_, err := api.InsertUser(ctx, csProject, "orders", &sqladmin.User{Name: principal, Type: "CLOUD_IAM_SERVICE_ACCOUNT"})
		return err
	}
	remove := func(ctx context.Context) error {
		_, err := api.DeleteUser(ctx, csProject, "orders", principal)
		return err
	}

	// Create: the user is inserted once, and the role is marked, limited and
	// without a password.
	if err := admin.ApplyCloudSQLIAMRole(ctx, params(pgclient.Owner), insert); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.f.counts(); n != 1 {
		t.Errorf("users.insert calls = %d", n)
	}
	st, err := admin.Role(ctx, principal)
	if err != nil || !st.Base.Exists || !st.Base.Login || st.Base.ConnLimit != 3 ||
		st.Marker != (pgclient.Marker{Claim: claim, Privileges: pgclient.Owner}) || st.A.Exists || st.B.Exists {
		t.Fatalf("after create: %+v, %v", st, err)
	}
	root := adminSession(t, admin, "")
	if got := one(t, root, "SELECT (rolpassword IS NULL)::text AS none FROM pg_authid WHERE rolname = "+pgclient.Literal(principal), "none"); got != "true" {
		t.Errorf("the IAM role has a password: none = %q", got)
	}

	// The role's own privileges: as an Owner it creates a table.
	as := adminSession(t, admin, db)
	if err := as.Exec(ctx, "SET ROLE "+pgclient.Ident(principal)); err != nil {
		t.Fatal(err)
	}
	if err := as.Exec(ctx, "CREATE TABLE public.mine (id int)"); err != nil {
		t.Errorf("Owner create table: %v", err)
	}

	// Applied again, the role is updated and the user is not inserted again.
	if err := admin.ApplyCloudSQLIAMRole(ctx, params(pgclient.ReadOnly), insert); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.f.counts(); n != 1 {
		t.Errorf("users.insert calls after an update = %d", n)
	}
	if st, err = admin.Role(ctx, principal); err != nil || st.Marker.Privileges != pgclient.ReadOnly {
		t.Errorf("after update: %+v, %v", st, err)
	}

	// Deleting reassigns what the role owns and deletes the user.
	skipped, err := admin.DropCloudSQLIAMRole(ctx, principal, claim, db, remove)
	if err != nil || skipped {
		t.Fatalf("drop: skipped %v, %v", skipped, err)
	}
	if _, d := h.f.counts(); d != 1 {
		t.Errorf("users.delete calls = %d", d)
	}
	if st, err = admin.Role(ctx, principal); err != nil || st.Base.Exists {
		t.Errorf("after drop: %+v, %v", st, err)
	}
	owner := one(t, adminSession(t, admin, db), "SELECT tableowner AS owner FROM pg_tables WHERE tablename = 'mine'", "owner")
	if owner != h.pg.user {
		t.Errorf("the table is owned by %q, want the admin %q", owner, h.pg.user)
	}
}

// A role of the principal's name that the claim did not create is refused
// before the Admin API is called, and left alone by a delete.
func TestCloudSQLIAMRoleNameCollision(t *testing.T) {
	ctx := context.Background()
	h := newCloudSQLHarness(t, egress.ModeAllowPrivate)
	admin := h.f.admin
	api := h.api(t)
	db := newDatabase(t, admin, "it/csqlx/db-uid")
	const claim = "it/csqlx/role-uid"
	root := adminSession(t, admin, "")
	called := 0
	insert := func(ctx context.Context) error {
		called++
		_, err := api.InsertUser(ctx, csProject, "orders", &sqladmin.User{Name: "x"})
		return err
	}
	for name, comment := range map[string]string{"unmarked": "", "foreign": "craftainer.dev/claim=someone/else/uid"} {
		principal := "it-" + suffix() + "-" + name + "@" + csProject + ".iam"
		t.Cleanup(func() { _ = root.Exec(ctx, "DROP ROLE IF EXISTS "+pgclient.Ident(principal)) })
		stmts := "CREATE ROLE " + pgclient.Ident(principal) + " LOGIN"
		if comment != "" {
			stmts += "; COMMENT ON ROLE " + pgclient.Ident(principal) + " IS " + pgclient.Literal(comment)
		}
		if err := root.Exec(ctx, stmts); err != nil {
			t.Fatal(err)
		}
		err := admin.ApplyCloudSQLIAMRole(ctx, pgclient.RoleParams{
			Base: principal, Database: db, ConnLimit: -1, Privileges: pgclient.ReadWrite, Marker: pgclient.Marker{Claim: claim},
		}, insert)
		if !errors.Is(err, pgclient.ErrNotOwned) {
			t.Errorf("%s: %v", name, err)
		}
		skipped, err := admin.DropCloudSQLIAMRole(ctx, principal, claim, db, func(context.Context) error {
			t.Errorf("%s: users.delete was called for a role that is not the claim's", name)
			return nil
		})
		if err != nil || !skipped {
			t.Errorf("%s: skipped %v, %v", name, skipped, err)
		}
		if st, _ := admin.Role(ctx, principal); !st.Base.Exists || st.Base.Marker.Claim == claim {
			t.Errorf("%s role was touched: %+v", name, st)
		}
	}
	if called != 0 {
		t.Errorf("users.insert was called %d times for roles that exist", called)
	}
}
