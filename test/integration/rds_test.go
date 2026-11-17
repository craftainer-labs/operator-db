//go:build integration && rds

package integration

// The AWS RDS backend (NKdb-1a) against moto server (RDS describe calls) and
// the TLS PostgreSQL of the other tests as the "RDS" data plane. Both are
// reached at a private container address, never loopback, with
// EGRESS_MODE=allow-private in this test's wiring only (pkg/egress blocks
// loopback in both modes). moto does not enforce IAM and PostgreSQL is not RDS:
// this checks the API shapes and the control-plane to data-plane path, not
// credentials (docs/nfrs/NFR-0005).

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
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
	opsNS     = "operator-db-system"
	motoRegn  = "us-east-1"
	tlsServer = "postgres" // a name in the test certificate; the address is a bare IP
)

// motoEndpoint is the moto server, from MOTO_ENDPOINT (http://<private ip>:5000).
func motoEndpoint(t *testing.T) string {
	t.Helper()
	v := os.Getenv("MOTO_ENDPOINT")
	if v == "" {
		t.Fatal("MOTO_ENDPOINT is not set; see test/integration/README.md")
	}
	return v
}

type rdsHarness struct {
	cfg  controller.Config
	conn libserver.Connection
	rds  *rds.Client
	pg   server
}

func newRDSHarness(t *testing.T, mode egress.Mode) *rdsHarness {
	t.Helper()
	pg := load(t)
	endpoint := motoEndpoint(t)
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws", Namespace: opsNS},
		Data:       map[string][]byte{cloud.KeyAWSAccessKeyID: []byte("test"), cloud.KeyAWSSecretAccessKey: []byte("test")},
	}
	h := &rdsHarness{pg: pg}
	h.cfg = controller.Config{
		Client: fake.NewClientBuilder().WithObjects(creds).Build(), OperatorNamespace: opsNS,
		Egress: egress.NewPolicy(mode), EgressMode: mode, RDS: controller.NewRDS, Connect: pgclient.Connect,
	}
	h.conn = libserver.Connection{
		Admin:    map[string][]byte{controller.AdminUsernameKey: []byte(pg.user), controller.AdminPasswordKey: []byte(pg.password)},
		CABundle: pg.ca, ServerName: tlsServer, MinTLSVersion: 0x0304,
	}
	// The test's own client is built the way the operator's are, through cloud.Egress.
	e := cloud.NewEgress(egress.ModeAllowPrivate, nil)
	acfg, err := e.AWS(context.Background(), cloud.ScopeCluster,
		cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: motoRegn}, EndpointOverride: endpoint},
		&cloud.Credentials{Source: cloud.SourceSecret}, creds.Data)
	if err != nil {
		t.Fatalf("AWS client: %v", err)
	}
	h.rds = rds.NewFromConfig(acfg)
	return h
}

// server is a cluster server for the instance id, pointing at moto and, as
// the data plane, at the PostgreSQL (spec.host overrides the address moto
// reports, which is not reachable).
func (h *rdsHarness) server(id, override string) *dbv1alpha1.ClusterDatabaseServer {
	s := &dbv1alpha1.ClusterDatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: id, Generation: 1}}
	s.Spec.Host, s.Spec.Port, s.Spec.AdminDatabase = h.pg.host, int32(h.pg.port), "postgres"
	s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderAWS, AWS: &dbv1alpha1.ClusterAWSDatabase{
		AWSClusterBlock: cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: motoRegn}, EndpointOverride: override},
		AWSTarget:       dbv1alpha1.AWSTarget{DBInstanceIdentifier: id},
	}}
	s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "aws", Namespace: opsNS}}
	s.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	return s
}

func (h *rdsHarness) createInstance(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	_, err := h.rds.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(id), DBInstanceClass: aws.String("db.t3.micro"), Engine: aws.String("postgres"),
		MasterUsername: aws.String("master"), MasterUserPassword: aws.String("master-pw-123"), AllocatedStorage: aws.Int32(5),
	})
	if err != nil {
		t.Fatalf("create the instance in moto: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.rds.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{DBInstanceIdentifier: aws.String(id), SkipFinalSnapshot: aws.Bool(true)})
	})
}

func TestRDSProbeDiscoversAndConnects(t *testing.T) {
	h := newRDSHarness(t, egress.ModeAllowPrivate)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := "orders-" + suffix()
	h.createInstance(t, id)

	srv := h.server(id, motoEndpoint(t))
	version, err := controller.Prober{Config: h.cfg}.Probe(ctx, srv, h.conn)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(version, "18") {
		t.Errorf("server_version = %q", version)
	}
	d := srv.Status.Discovered
	if d[dbv1alpha1.DiscoveredEndpoint] == "" || d[dbv1alpha1.DiscoveredResourceID] == "" ||
		d[dbv1alpha1.DiscoveredRegion] != motoRegn || d[dbv1alpha1.DiscoveredAccountID] == "" {
		t.Errorf("discovered = %v", d)
	}
	if c := meta.FindStatusCondition(srv.Status.Conditions, controller.ConditionCloudResolved); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("CloudResolved = %+v", c)
	}
}

func TestRDSProbeFailuresAreCategories(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// An instance moto does not have.
	h := newRDSHarness(t, egress.ModeAllowPrivate)
	_, err := controller.Prober{Config: h.cfg}.Probe(ctx, h.server("absent-instance", motoEndpoint(t)), h.conn)
	var ce *controller.CloudError
	if !errors.As(err, &ce) || ce.Reason != controller.ReasonTargetNotFound {
		t.Errorf("missing instance: %v", err)
	}

	// endpointOverride is refused in strict mode, before any request is made.
	strict := newRDSHarness(t, egress.ModeStrict)
	_, err = controller.Prober{Config: strict.cfg}.Probe(ctx, strict.server("x", motoEndpoint(t)), strict.conn)
	if !errors.As(err, &ce) || ce.Reason != controller.ReasonEndpointOverrideForbidden {
		t.Errorf("override in strict mode: %v", err)
	}
}
