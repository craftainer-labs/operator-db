package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

const (
	// entraAdmin is the Entra principal behind the operator's credentials.
	entraAdmin = "operator@contoso.example"
	// azPrincipal is the consumer's Entra principal of an IAM-mode claim.
	azPrincipal = "app@contoso.example"
)

func azureCreds() map[string][]byte {
	return map[string][]byte{cloud.KeyAzureClientID: []byte("11111111-1111-4111-8111-111111111111"),
		cloud.KeyAzureClientSecret: []byte("entra-client-secret")}
}

// entraHarness is an Azure harness whose server logs in as the Entra
// administrator: adminAuthMode IAM, entraAdmin and no admin Secret.
func entraHarness(t *testing.T, f *fakeARM) *harness {
	t.Helper()
	h := azureHarness(t, f)
	h.get(h.server)
	h.server.Spec.AdminCredentialsSecretRef = nil
	h.server.Spec.AdminAuthMode = dbv1alpha1.AdminAuthModeIAM
	h.server.Spec.Provider.Azure.EntraAdmin = entraAdmin
	h.must(env.Client.Update(context.Background(), h.server))
	return h
}

func azureIAMClaim(principal string) func(*dbv1alpha1.DatabaseRole) {
	return func(r *dbv1alpha1.DatabaseRole) {
		r.Spec.AuthMode = dbv1alpha1.AuthModeIAM
		if principal != "" {
			r.Spec.IAM = &dbv1alpha1.IAMSpec{Principal: principal}
		}
	}
}

func TestValidEntraPrincipal(t *testing.T) {
	for p, want := range map[string]bool{
		"app@contoso.example":     true,
		"My Managed Identity":     true,
		"svc-orders":              true,
		"":                        false,
		" app@contoso.example":    false,
		"app@contoso.example ":    false,
		"app\n@contoso.example":   false,
		"app\x00":                 false,
		"\xff\xfe":                false,
		strings.Repeat("a", 63):   true,
		strings.Repeat("a", 64):   false,
		strings.Repeat("é", 32):   false,
		"app@contoso.example\t":   false,
		"app@contoso.example\x7f": false,
	} {
		if got := ValidEntraPrincipal(p); got != want {
			t.Errorf("ValidEntraPrincipal(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestAzureAdminIAMCloudOf(t *testing.T) {
	s := &dbv1alpha1.ClusterDatabaseServer{}
	s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderAzure, Azure: &dbv1alpha1.ClusterAzureDatabase{EntraAdmin: entraAdmin}}
	if cs := cloudOf(s); cs.adminIAM || cs.entraAdmin != entraAdmin {
		t.Errorf("Password mode: %+v", cs)
	}
	s.Spec.AdminAuthMode = dbv1alpha1.AdminAuthModeIAM
	if cs := cloudOf(s); !cs.adminIAM || cs.entraAdmin != entraAdmin {
		t.Errorf("IAM mode: %+v", cs)
	}
	if (Config{}).entraLogin(nil, server.CloudConnection{}) != nil || (Config{}).entraLogin(&cloudSpec{}, server.CloudConnection{}) != nil {
		t.Error("a server without adminAuthMode IAM has an Entra login")
	}
}

// The Entra token comes from the operator's own cloud credentials, for the
// scope of Azure Database for PostgreSQL, through the egress policy; a refusal
// is a category.
func TestEntraSource(t *testing.T) {
	f := newFakeARM(t)
	h := entraHarness(t, f)
	ctx := context.Background()
	cs := cloudOf(h.server)
	cc := server.CloudConnection{Credentials: azureCreds()}

	tok, err := h.cfg.entraSource(cs, cc)(ctx)
	if err != nil || tok != "fake-entra-token" {
		t.Fatalf("token = %q, %v", tok, err)
	}

	// The Entra authority refuses the credential.
	f.mu.Lock()
	f.entra = 401
	f.mu.Unlock()
	_, err = h.cfg.entraSource(cs, cc)(ctx)
	if !errors.Is(err, cloud.ErrCredentialsUnavailable) || strings.Contains(err.Error(), azSecretText) {
		t.Errorf("refused: %v", err)
	}
	f.mu.Lock()
	f.entra = 0
	f.mu.Unlock()

	// A bad credentials Secret, and an override that strict mode forbids.
	_, err = h.cfg.entraSource(cs, server.CloudConnection{Credentials: map[string][]byte{cloud.KeyAzureClientID: []byte("x")}})(ctx)
	if reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("bad Secret: %v", err)
	}
	strict := h.cfg
	strict.EgressMode = egress.ModeStrict
	if _, err = strict.entraSource(cs, cc)(ctx); reasonOf(t, err) != ReasonEndpointOverrideForbidden {
		t.Errorf("override in strict mode: %v", err)
	}
}

// With adminAuthMode IAM the claims' admin connection and the probe log in as
// the Entra administrator, with a token as the password and no admin Secret.
func TestAzureEntraAdminLogin(t *testing.T) {
	f := newFakeARM(t)
	h := entraHarness(t, f)
	ctx := context.Background()
	h.fake.answer = versionAnswer

	be, err := h.cfg.backend(ctx, h.ns, h.ref())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if _, err = be.admin.ServerVersion(ctx); err != nil || len(h.fake.targets) != 1 {
		t.Fatalf("admin session: %v, %d connections", err, len(h.fake.targets))
	}
	tg := h.fake.targets[0]
	if tg.User != entraAdmin || tg.Password != "" || tg.Token == nil || tg.Host != azFQDN {
		t.Fatalf("target = %+v", tg)
	}
	if tok, err := tg.Token(ctx); err != nil || tok != "fake-entra-token" {
		t.Errorf("token = %q, %v", tok, err)
	}

	// The probe does the same, with the credentials the connection carries.
	fake := &pgFake{answer: versionAnswer}
	cfg := h.cfg
	cfg.Connect = fake.connect
	srv := &dbv1alpha1.ClusterDatabaseServer{}
	h.get(h.server)
	h.server.DeepCopyInto(srv)
	srv.Status.Discovered = nil
	res, err := Prober{Config: cfg}.ProbeCloud(ctx, srv, server.CloudConnection{
		Connection: server.Connection{CABundle: []byte("ca")}, Credentials: azureCreds()})
	if err != nil || res.Version == "" || res.Discovered[dbv1alpha1.DiscoveredEndpoint] != azFQDN || len(fake.targets) != 1 {
		t.Fatalf("probe: %+v, %v, %d connections", res, err, len(fake.targets))
	}
	if tg = fake.targets[0]; tg.User != entraAdmin || tg.Password != "" || tg.Token == nil {
		t.Errorf("probe target = %+v", tg)
	}

	// Password mode still needs the Secret.
	if _, err = adminTarget(dbv1alpha1.PostgreSQL{Host: "h"}, nil, server.Connection{}, nil, nil); !errors.Is(err, ErrAdminCredentials) {
		t.Errorf("no admin Secret in Password mode: %v", err)
	}
}

// An IAM-mode claim on Azure is an Entra principal role that
// pgaadauth_create_principal creates, named by the principal, with the marker,
// the grants and no password; its binding has no password and no uri; nothing
// rotates; deleting reassigns the objects and drops the role.
func TestAzureIAMRoleLifecycle(t *testing.T) {
	f := newFakeARM(t)
	h := azureHarness(t, f)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	dbName := pgclient.DatabaseName(h.ns, "orders")
	role := h.role("orders-iam", "orders", azureIAMClaim(azPrincipal))
	q := `"` + azPrincipal + `"`

	h.fake.reset()
	h.reconcileOK(rr, role)
	h.get(role)
	claim := h.ns + "/orders-iam/" + string(role.UID)
	h.fake.want(t,
		`|SELECT pgaadauth_create_principal('`+azPrincipal+`', false, false)`,
		`|COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=`+claim+`'`,
		`|GRANT CONNECT ON DATABASE "`+dbName+`" TO `+q,
		`|COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=`+claim+` privileges=ReadWrite'`)
	h.fake.wantNot(t, "CREATE ROLE", "PASSWORD", `"rds_iam"`, "NOLOGIN", "SET role")
	if role.Status.ExternalName != azPrincipal || role.Status.Rotation != nil {
		t.Errorf("status = %+v", role.Status)
	}
	data := bindingSecret(h, role)
	for k, v := range map[string]string{
		"type": "postgresql", "provider": "craftainer.dev", "cloud": "azure", "authMode": "IAM",
		"host": azFQDN, "port": "5432", "database": dbName, "username": azPrincipal, "sslmode": "verify-full",
	} {
		if got, ok := data[k]; !ok || got != v {
			t.Errorf("binding[%s] = %q (present %v), want %q", k, got, ok, v)
		}
	}
	for _, k := range []string{"password", "uri", "region", "iamResourceArn", "instanceConnectionName"} {
		if _, ok := data[k]; ok {
			t.Errorf("the binding has %q: %v", k, data)
		}
	}

	// Steady state: nothing is written.
	h.fake.answer = iamRoleAnswer(azPrincipal, claim, "ReadWrite")
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.wantNot(t, "CREATE", "ALTER", "COMMENT", "GRANT", "DROP", "pgaadauth")

	// A role of that name that is someone else's is never touched.
	h.fake.answer = iamRoleAnswer(azPrincipal, "other/role/uid", "ReadOnly")
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.get(role)
	h.wantCondition(role.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	h.fake.wantNot(t, "ALTER", "GRANT", "DROP", "COMMENT", "pgaadauth")

	// Deleting hands the objects to the admin role and drops the role.
	h.fake.answer = iamRoleAnswer(azPrincipal, claim, "ReadWrite")
	h.must(env.Client.Delete(context.Background(), role))
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `pg_terminate_backend(pid)`, `|GRANT `+q+` TO CURRENT_USER`, dbName+`|REASSIGN OWNED BY `+q+` TO CURRENT_USER`,
		dbName+`|DROP OWNED BY `+q, `|DROP ROLE IF EXISTS `+q, `|REVOKE `+q+` FROM CURRENT_USER`)
	if !h.gone(role) {
		t.Error("the claim is still there")
	}
}

// A missing or malformed principal is refused before anything is created, and
// the text names no principal.
func TestAzureIAMPrincipalChecks(t *testing.T) {
	f := newFakeARM(t)
	h := azureHarness(t, f)
	rr := &RoleReconciler{h.cfg}
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))
	for name, principal := range map[string]string{
		"none": "", "padded": " app@contoso.example", "control": "app\u0001@contoso.example", "too long": strings.Repeat("a", 64),
	} {
		h.rec.Reset()
		h.fake.reset()
		role := h.role("iam-"+strings.ReplaceAll(name, " ", "-"), "orders", azureIAMClaim(principal))
		if err := h.reconcile(rr, role); !errors.Is(err, ErrIAMPrincipalInvalidEntra) || !strings.Contains(err.Error(), ReasonIAMPrincipalRequired) {
			t.Errorf("%s: %v", name, err)
		}
		h.rec.AssertEvent(t, "Warning", ReasonIAMPrincipalRequired, "")
		h.fake.wantNot(t, "pgaadauth", "COMMENT", "GRANT")
	}
}
