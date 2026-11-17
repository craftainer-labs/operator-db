package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

const (
	iamAccount  = "111122223333"
	iamResource = "db-ABC"
)

// cloudHarness is a harness whose server is an AWS one that the probe has
// resolved, with the discovered account and resource id of an IAM login's ARN.
func cloudHarness(t *testing.T, policy common.DeletionPolicy) *harness {
	t.Helper()
	h := newHarness(t, policy, func(s *dbv1alpha1.ClusterDatabaseServer) {
		s.Spec.Host, s.Spec.Port = "", 0
		s.Spec.Provider = awsServer("eu-west-1").Spec.Provider
		s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret,
			SecretRef: &cloud.SecretRef{Name: "aws", Namespace: s.Spec.AdminCredentialsSecretRef.Namespace}}
	})
	h.get(h.server)
	h.server.Status.Discovered = map[string]string{
		"endpoint": "orders.abc.rds.test", "port": "5433", "region": "eu-west-1",
		dbv1alpha1.DiscoveredAccountID: iamAccount, dbv1alpha1.DiscoveredResourceID: iamResource,
	}
	h.must(env.Client.Status().Update(context.Background(), h.server))
	return h
}

func iamClaim(r *dbv1alpha1.DatabaseRole) { r.Spec.AuthMode = dbv1alpha1.AuthModeIAM }

// iamRoleAnswer answers the role query with the one login role of an IAM claim.
func iamRoleAnswer(base, claim, privileges string) func(string) []pgclient.Row {
	return func(sql string) []pgclient.Row {
		if !strings.Contains(sql, "FROM pg_roles") {
			return nil
		}
		return []pgclient.Row{{"name": base, "login": "true", "connlimit": "-1", "rdsiam": "true",
			"comment": pgclient.Marker{Claim: claim, Privileges: privileges}.String()}}
	}
}

// An IAM-mode claim on an AWS server is one login role with rds_iam and no
// password; its binding has the ARN and no password; nothing rotates; deleting
// drops the role.
func TestIAMRoleLifecycle(t *testing.T) {
	h := cloudHarness(t, common.DeletionDelete)
	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	dbName := pgclient.DatabaseName(h.ns, "orders")
	role := h.role("orders-iam", "orders", iamClaim)
	base := pgclient.RoleBase(h.ns, "orders-iam")

	h.fake.reset()
	h.reconcileOK(rr, role)
	h.get(role)
	claim := h.ns + "/orders-iam/" + string(role.UID)
	q := `"` + base + `"`
	h.fake.want(t,
		`|CREATE ROLE `+q+` LOGIN; COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=`+claim+`'`,
		`|ALTER ROLE `+q+` WITH LOGIN PASSWORD NULL CONNECTION LIMIT -1`,
		`|GRANT "rds_iam" TO `+q,
		`|GRANT CONNECT ON DATABASE "`+dbName+`" TO `+q,
		dbName+`|GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+q,
		`|COMMENT ON ROLE `+q+` IS 'craftainer.dev/claim=`+claim+` privileges=ReadWrite'`)
	h.fake.wantNot(t, `"`+base+`_a"`, `"`+base+`_b"`, "NOLOGIN", "SET role")
	if strings.Contains(strings.ReplaceAll(h.fake.joined(), "PASSWORD NULL", ""), "PASSWORD") {
		t.Errorf("a password is set:\n%s", h.fake.joined())
	}

	data := bindingSecret(h, role)
	arn := "arn:aws:rds-db:eu-west-1:" + iamAccount + ":dbuser:" + iamResource + "/" + base
	for k, v := range map[string]string{
		"type": "postgresql", "provider": "craftainer.dev", "cloud": "aws", "authMode": "IAM", "region": "eu-west-1",
		"host": "orders.abc.rds.test", "port": "5433", "database": dbName, "username": base, "sslmode": "verify-full",
		"iamResourceArn": arn,
	} {
		if got, ok := data[k]; !ok || got != v {
			t.Errorf("binding[%s] = %q (present %v), want %q", k, got, ok, v)
		}
	}
	for _, k := range []string{"password", "uri"} {
		if _, ok := data[k]; ok {
			t.Errorf("the binding has %q: %v", k, data)
		}
	}
	if _, ok := data["sslrootcert"]; !ok {
		t.Errorf("the binding has no sslrootcert: %v", data)
	}
	if role.Status.Rotation != nil || role.Status.ExternalName != base {
		t.Errorf("status = %+v", role.Status)
	}

	// Steady state: nothing is written.
	h.fake.answer = iamRoleAnswer(base, claim, "ReadWrite")
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.wantNot(t, "CREATE", "ALTER", "COMMENT", "GRANT", "DROP")

	// The rotate annotation is a Warning Event, and nothing rotates.
	h.get(role)
	patch := client.MergeFrom(role.DeepCopy())
	role.Annotations = map[string]string{lifecycle.AnnotationRotate: "1"}
	h.must(env.Client.Patch(context.Background(), role, patch))
	h.rec.Reset()
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.rec.AssertEvent(t, "Warning", ReasonRotationNotApplicable, "no credential to rotate")
	h.fake.wantNot(t, "CREATE", "ALTER", "COMMENT", "GRANT", "DROP")
	h.get(role)
	if role.Status.Rotation != nil {
		t.Errorf("rotation = %+v", role.Status.Rotation)
	}
	if bindingSecret(h, role)["iamResourceArn"] != arn {
		t.Error("the binding changed")
	}

	// Drift: the privileges changed; the role is updated, not created, and the
	// binding is the same.
	patch = client.MergeFrom(role.DeepCopy())
	role.Spec.Privileges = dbv1alpha1.PrivilegesReadOnly
	h.must(env.Client.Patch(context.Background(), role, patch))
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `|GRANT "rds_iam" TO `+q, dbName+`|GRANT SELECT ON ALL TABLES IN SCHEMA public TO `+q,
		`privileges=ReadOnly'`)
	h.fake.wantNot(t, "CREATE ROLE")

	// A role of that name that is someone else's is never touched.
	h.fake.answer = iamRoleAnswer(base, "other/role/uid", "ReadOnly")
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.get(role)
	h.wantCondition(role.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	h.fake.wantNot(t, "CREATE", "ALTER", "GRANT", "DROP")

	// Deleting drops the one role, and the sessions and objects it has.
	h.fake.answer = iamRoleAnswer(base, claim, "ReadOnly")
	h.must(env.Client.Delete(context.Background(), role))
	h.fake.reset()
	h.reconcileOK(rr, role)
	h.fake.want(t, `pg_terminate_backend(pid)`, dbName+`|REASSIGN OWNED BY `+q+` TO CURRENT_USER`, `|DROP ROLE IF EXISTS `+q)
	if !h.gone(role) {
		t.Error("the claim is still there")
	}
}

// The ARN needs what the probe discovered, and a claim whose Database cannot
// be used creates nothing.
func TestIAMRolePreconditions(t *testing.T) {
	h := cloudHarness(t, common.DeletionRetain)
	rr := &RoleReconciler{h.cfg}
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))

	missing := h.role("no-db", "absent", iamClaim)
	if err := h.reconcile(rr, missing); !errors.Is(err, ErrDatabaseNotUsable) {
		t.Errorf("missing database: %v", err)
	}

	h.get(h.server)
	delete(h.server.Status.Discovered, dbv1alpha1.DiscoveredAccountID)
	h.must(env.Client.Status().Update(context.Background(), h.server))
	role := h.role("orders-iam", "orders", iamClaim)
	h.fake.reset()
	if err := h.reconcile(rr, role); !errors.Is(err, ErrIAMTargetUnknown) {
		t.Errorf("undiscovered account: %v", err)
	}
	h.fake.wantNot(t, "CREATE", "GRANT")

	for name, tc := range map[string]struct {
		region string
		d      map[string]string
	}{
		"no region":   {"", map[string]string{"accountId": "1", "resourceId": "r"}},
		"no account":  {"eu-west-1", map[string]string{"resourceId": "r"}},
		"no resource": {"eu-west-1", map[string]string{"accountId": "1"}},
		"nothing":     {"", nil},
	} {
		if _, err := IAMResourceARN(tc.region, tc.d, "u"); !errors.Is(err, ErrIAMTargetUnknown) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if arn, err := IAMResourceARN("r1", map[string]string{"accountId": "1", "resourceId": "db-X"}, "u"); err != nil ||
		arn != "arn:aws:rds-db:r1:1:dbuser:db-X/u" {
		t.Errorf("arn = %q, %v", arn, err)
	}
}

// IAM needs a cloud server: a self-hosted one refuses the claim with its
// reason and creates nothing.
func TestIAMRoleOnASelfHostedServer(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	rr := &RoleReconciler{h.cfg}
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))
	h.fake.reset()
	h.rec.Reset()
	role := h.role("orders-iam", "orders", iamClaim)
	if err := h.reconcile(rr, role); err == nil || !strings.Contains(err.Error(), ReasonAuthModeUnsupported) {
		t.Errorf("IAM: %v", err)
	}
	h.rec.AssertEvent(t, "Warning", ReasonAuthModeUnsupported, "")
	h.fake.wantNot(t, "CREATE ROLE", "GRANT")
}

// A failing apply is the claim's error, and no binding is written.
func TestIAMRoleApplyFailure(t *testing.T) {
	h := cloudHarness(t, common.DeletionRetain)
	rr := &RoleReconciler{h.cfg}
	h.reconcileOK(&DatabaseReconciler{h.cfg}, h.database("orders", nil))
	role := h.role("orders-iam", "orders", iamClaim)
	h.fake.failOn = "rds_iam"
	if err := h.reconcile(rr, role); !errors.Is(err, errStatement) {
		t.Errorf("failing grant: %v", err)
	}
	h.get(role)
	if role.Status.Binding != nil {
		t.Errorf("a binding was written: %+v", role.Status.Binding)
	}
}
