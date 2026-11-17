package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
)

// fakeRDS answers the two describe calls.
type fakeRDS struct {
	inst    *rds.DescribeDBInstancesOutput
	cluster *rds.DescribeDBClustersOutput
	err     error
	calls   int
}

func (f *fakeRDS) DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	f.calls++
	return f.inst, f.err
}

func (f *fakeRDS) DescribeDBClusters(context.Context, *rds.DescribeDBClustersInput, ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error) {
	f.calls++
	return f.cluster, f.err
}

func instanceOut() *rds.DescribeDBInstancesOutput {
	return &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{
		Endpoint:      &rdstypes.Endpoint{Address: aws.String("orders.abc.eu-west-1.rds.amazonaws.com"), Port: aws.Int32(5433)},
		DbiResourceId: aws.String("db-ABC"), DBInstanceArn: aws.String("arn:aws:rds:eu-west-1:111122223333:db:orders"),
	}}}
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var ce *CloudError
	if !errors.As(err, &ce) {
		t.Fatalf("not a CloudError: %v", err)
	}
	return ce.Reason
}

func TestDescribeInstanceAndCluster(t *testing.T) {
	ctx := context.Background()
	got, err := describe(ctx, &fakeRDS{inst: instanceOut()}, dbv1alpha1.AWSTarget{DBInstanceIdentifier: "orders"}, "eu-west-1")
	want := map[string]string{"endpoint": "orders.abc.eu-west-1.rds.amazonaws.com", "port": "5433", "resourceId": "db-ABC",
		"accountId": "111122223333", "region": "eu-west-1"}
	if err != nil || len(got) != len(want) {
		t.Fatalf("instance: %v, %v", got, err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("instance %s = %q, want %q", k, got[k], v)
		}
	}
	cl := &rds.DescribeDBClustersOutput{DBClusters: []rdstypes.DBCluster{{
		Endpoint: aws.String("aurora.cluster-abc.eu-west-1.rds.amazonaws.com"), Port: aws.Int32(5432),
		DbClusterResourceId: aws.String("cluster-XYZ"), DBClusterArn: aws.String("arn:aws:rds:eu-west-1:111122223333:cluster:aurora"),
	}}}
	got, err = describe(ctx, &fakeRDS{cluster: cl}, dbv1alpha1.AWSTarget{DBClusterIdentifier: "aurora"}, "eu-west-1")
	if err != nil || got["resourceId"] != "cluster-XYZ" || got["endpoint"] != "aurora.cluster-abc.eu-west-1.rds.amazonaws.com" {
		t.Errorf("cluster: %v, %v", got, err)
	}
	// A bare answer: only the endpoint is required.
	bare := &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{Endpoint: &rdstypes.Endpoint{Address: aws.String("h")}}}}
	got, err = describe(ctx, &fakeRDS{inst: bare}, dbv1alpha1.AWSTarget{DBInstanceIdentifier: "x"}, "r")
	if err != nil || len(got) != 2 {
		t.Errorf("bare: %v, %v", got, err)
	}
}

func TestDescribeFailuresAreCategories(t *testing.T) {
	ctx := context.Background()
	inst, cluster := dbv1alpha1.AWSTarget{DBInstanceIdentifier: "x"}, dbv1alpha1.AWSTarget{DBClusterIdentifier: "x"}
	secret := errors.New("AKIASECRET denied for arn:aws:iam::1:role/x")
	for name, tc := range map[string]struct {
		api    *fakeRDS
		target dbv1alpha1.AWSTarget
		reason string
	}{
		"instance missing":  {&fakeRDS{err: &rdstypes.DBInstanceNotFoundFault{}}, inst, ReasonTargetNotFound},
		"instance empty":    {&fakeRDS{inst: &rds.DescribeDBInstancesOutput{}}, inst, ReasonTargetNotFound},
		"cluster missing":   {&fakeRDS{err: &rdstypes.DBClusterNotFoundFault{}}, cluster, ReasonTargetNotFound},
		"cluster empty":     {&fakeRDS{cluster: &rds.DescribeDBClustersOutput{}}, cluster, ReasonTargetNotFound},
		"instance api":      {&fakeRDS{err: secret}, inst, ReasonCloudError},
		"cluster api":       {&fakeRDS{err: secret}, cluster, ReasonCloudError},
		"no endpoint":       {&fakeRDS{inst: &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{}}}}, inst, ReasonCloudError},
		"cluster no endpnt": {&fakeRDS{cluster: &rds.DescribeDBClustersOutput{DBClusters: []rdstypes.DBCluster{{}}}}, cluster, ReasonCloudError},
	} {
		_, err := describe(ctx, tc.api, tc.target, "r")
		if got := reasonOf(t, err); got != tc.reason {
			t.Errorf("%s: reason %s, want %s", name, got, tc.reason)
		}
		if strings.Contains(err.Error(), "AKIA") || strings.Contains(err.Error(), "arn:") {
			t.Errorf("%s: the SDK error text leaked: %v", name, err)
		}
	}
}

func awsServer(region string) *dbv1alpha1.ClusterDatabaseServer {
	s := &dbv1alpha1.ClusterDatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "rds"}}
	s.Spec.Provider = &dbv1alpha1.ClusterProvider{Type: cloud.ProviderAWS, AWS: &dbv1alpha1.ClusterAWSDatabase{
		AWSClusterBlock: cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: region}},
		AWSTarget:       dbv1alpha1.AWSTarget{DBInstanceIdentifier: "orders"},
	}}
	return s
}

func TestCloudOf(t *testing.T) {
	if cloudOf(&dbv1alpha1.ClusterDatabaseServer{}) != nil || cloudOf(&dbv1alpha1.DatabaseServer{}) != nil {
		t.Error("a self-hosted server has a cloud spec")
	}
	if cloudOf(&dbv1alpha1.ClusterDatabaseServer{Spec: dbv1alpha1.ClusterDatabaseServerSpec{Provider: &dbv1alpha1.ClusterProvider{}}}) != nil ||
		cloudOf(&dbv1alpha1.DatabaseServer{Spec: dbv1alpha1.DatabaseServerSpec{Provider: &dbv1alpha1.Provider{}}}) != nil {
		t.Error("a provider without a block has a cloud spec")
	}
	if regionOf(nil) != "" || firstNonEmpty("", "b") != "b" || firstNonEmpty("a", "b") != "a" {
		t.Error("regionOf, firstNonEmpty")
	}
	if cloudOf(nil) != nil {
		t.Error("an unknown kind has a cloud spec")
	}
	c := awsServer("eu-west-1")
	c.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	cs := cloudOf(c)
	if cs.scope != cloud.ScopeCluster || cs.block.Region != "eu-west-1" || cs.target.DBInstanceIdentifier != "orders" || cs.creds == nil {
		t.Errorf("cluster = %+v", cs)
	}
	n := &dbv1alpha1.DatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "rds", Namespace: "team"}}
	n.Spec.Provider = &dbv1alpha1.Provider{Type: cloud.ProviderAWS, AWS: &dbv1alpha1.AWSDatabase{
		AWSBlock: cloud.AWSBlock{Region: "us-east-1"}, AWSTarget: dbv1alpha1.AWSTarget{DBClusterIdentifier: "a"}}}
	n.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "k"}}
	cs = cloudOf(n)
	if cs.scope != cloud.ScopeNamespaced || cs.ns != "team" || cs.block.EndpointOverride != "" || cs.creds.SecretRef.Name != "k" {
		t.Errorf("namespaced = %+v", cs)
	}
}

func TestCloudSecret(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	ctx := context.Background()
	h.must(env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "aws", Namespace: h.ops},
		Data: map[string][]byte{"accessKeyId": []byte("AKIAX"), "secretAccessKey": []byte("s")}}))
	h.must(env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "aws", Namespace: h.ns},
		Data: map[string][]byte{"accessKeyId": []byte("AKIAN"), "secretAccessKey": []byte("s")}}))
	secretCS := func(scope cloud.Scope, ns, refNS, name string) *cloudSpec {
		return &cloudSpec{scope: scope, ns: ns, creds: &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: name, Namespace: refNS}}}
	}
	for name, cs := range map[string]*cloudSpec{
		"no credentials":    {},
		"workload identity": {creds: &cloud.Credentials{Source: cloud.SourceWorkloadIdentity}},
	} {
		if got, err := h.cfg.cloudSecret(ctx, cs); got != nil || err != nil {
			t.Errorf("%s: %v, %v", name, got, err)
		}
	}
	got, err := h.cfg.cloudSecret(ctx, secretCS(cloud.ScopeCluster, "", h.ops, "aws"))
	if err != nil || string(got["accessKeyId"]) != "AKIAX" {
		t.Errorf("cluster: %v, %v", got, err)
	}
	got, err = h.cfg.cloudSecret(ctx, secretCS(cloud.ScopeNamespaced, h.ns, "", "aws"))
	if err != nil || string(got["accessKeyId"]) != "AKIAN" {
		t.Errorf("namespaced: %v, %v", got, err)
	}
	// A cluster server may not name a tenant's Secret.
	if _, err = h.cfg.cloudSecret(ctx, secretCS(cloud.ScopeCluster, "", h.ns, "aws")); reasonOf(t, err) != ReasonCredentialsNamespaceForbidden {
		t.Errorf("another namespace: %v", err)
	}
	if _, err = h.cfg.cloudSecret(ctx, secretCS(cloud.ScopeCluster, "", h.ops, "absent")); reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("missing Secret: %v", err)
	}
	// An API server failure is transient (the discovered address is kept).
	wc, cerr := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(cerr)
	flaky := h.cfg
	flaky.Client = interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewServiceUnavailable("api server down")
		},
	})
	_, err = flaky.cloudSecret(ctx, secretCS(cloud.ScopeCluster, "", h.ops, "aws"))
	var ce *CloudError
	if !errors.As(err, &ce) || !ce.transient || ce.Reason != ReasonCredentialsUnavailable {
		t.Errorf("API failure: %v", err)
	}
	// Forbidden is not transient.
	flaky.Client = interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewForbidden(corev1.Resource("secrets"), "aws", errors.New("no"))
		},
	})
	_, err = flaky.cloudSecret(ctx, secretCS(cloud.ScopeCluster, "", h.ops, "aws"))
	if !errors.As(err, &ce) || ce.transient {
		t.Errorf("forbidden: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	ctx := context.Background()
	h.must(env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "aws", Namespace: h.ops},
		Data: map[string][]byte{"accessKeyId": []byte("AKIAX"), "secretAccessKey": []byte("s")}}))
	h.must(env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: h.ops},
		Data: map[string][]byte{"accessKeyId": []byte("AKIAX")}}))
	if d, err := h.cfg.discover(ctx, nil, server.Connection{}); d != nil || err != nil {
		t.Errorf("self-hosted: %v, %v", d, err)
	}
	api := &fakeRDS{inst: instanceOut()}
	h.cfg.EgressMode = egress.ModeStrict
	h.cfg.RDS = func(cfg aws.Config) RDSAPI {
		if cfg.Region != "eu-west-1" {
			t.Errorf("region = %q", cfg.Region)
		}
		return api
	}
	cs := func(secret, override string) *cloudSpec {
		s := cloudOf(awsServer("eu-west-1"))
		s.block.EndpointOverride = override
		s.creds = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: secret, Namespace: h.ops}}
		return s
	}
	d, err := h.cfg.discover(ctx, cs("aws", ""), server.Connection{})
	if err != nil || d["endpoint"] != "orders.abc.eu-west-1.rds.amazonaws.com" || api.calls != 1 {
		t.Fatalf("discover: %v, %v", d, err)
	}
	if _, err = h.cfg.discover(ctx, cs("aws", "http://moto.example.test:5000"), server.Connection{}); reasonOf(t, err) != ReasonEndpointOverrideForbidden {
		t.Errorf("override in strict mode: %v", err)
	}
	if _, err = h.cfg.discover(ctx, cs("bad", ""), server.Connection{}); reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("malformed Secret: %v", err)
	}
	if _, err = h.cfg.discover(ctx, cs("absent", ""), server.Connection{}); reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("missing Secret: %v", err)
	}
	// A namespaced server cannot use WorkloadIdentity, whatever the CRD says.
	wi := cloudOf(&dbv1alpha1.DatabaseServer{Spec: dbv1alpha1.DatabaseServerSpec{Provider: &dbv1alpha1.Provider{Type: cloud.ProviderAWS,
		AWS: &dbv1alpha1.AWSDatabase{AWSBlock: cloud.AWSBlock{Region: "eu-west-1"}}}}})
	wi.creds = &cloud.Credentials{Source: cloud.SourceWorkloadIdentity}
	if _, err = h.cfg.discover(ctx, wi, server.Connection{}); reasonOf(t, err) != ReasonCredentialsUnavailable {
		t.Errorf("namespaced WorkloadIdentity: %v", err)
	}
	// A client that is not configured is a category, not a panic.
	noRDS := h.cfg
	noRDS.RDS = nil
	if _, err = noRDS.discover(ctx, cs("aws", ""), server.Connection{}); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("no RDS client: %v", err)
	}
	// An endpoint that is not a host name is refused.
	api.inst = &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{Endpoint: &rdstypes.Endpoint{Address: aws.String("evil host/x")}}}}
	if _, err = h.cfg.discover(ctx, cs("aws", ""), server.Connection{}); reasonOf(t, err) != ReasonCloudError {
		t.Errorf("hostile endpoint: %v", err)
	}
	api.inst = instanceOut()
	for h, want := range map[string]bool{"a.b-c.example": true, "2130706433": false, "a.0x7f": false, "a.123": false, "": false, "127.0.0.1": false, "::1": false, "a..b": false, "-a.b": false, "a-.b": false, strings.Repeat("a", 64): false, "a b": false, "a/b": false, "h:1": false, strings.Repeat("a", 254): false} {
		if validEndpoint(h) != want {
			t.Errorf("validEndpoint(%q) = %v", h, !want)
		}
	}
	// With the real constructor the client is the SDK's (no call is made).
	if NewRDS(aws.Config{Region: "eu-west-1"}) == nil {
		t.Error("NewRDS")
	}
}

func TestRecordAndStatusOf(t *testing.T) {
	srv := awsServer("eu-west-1")
	srv.Generation = 3
	record(srv, cloudOf(srv), map[string]string{"endpoint": "h"}, nil)
	st := serverStatusOf(srv)
	c := meta.FindStatusCondition(st.Conditions, ConditionCloudResolved)
	if st.Discovered["endpoint"] != "h" || c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonResolved || c.ObservedGeneration != 3 {
		t.Errorf("success: %+v %+v", st.Discovered, c)
	}
	record(srv, cloudOf(srv), nil, cloudErr(ReasonTargetNotFound, "the DB instance does not exist"))
	c = meta.FindStatusCondition(st.Conditions, ConditionCloudResolved)
	if st.Discovered != nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonTargetNotFound {
		t.Errorf("failure: %+v %+v", st.Discovered, c)
	}
	// A transient failure keeps the last address while the spec is the same,
	// and drops it when the spec changed; a plain error drops it.
	st.Discovered = map[string]string{"endpoint": "h"}
	record(srv, cloudOf(srv), nil, transientErr(ReasonCloudError, "x"))
	record(srv, cloudOf(srv), nil, transientErr(ReasonCloudError, "x"))
	if c = meta.FindStatusCondition(st.Conditions, ConditionCloudResolved); c.Reason != ReasonCloudError || st.Discovered["endpoint"] != "h" {
		t.Errorf("transient: %+v %v", c, st.Discovered)
	}
	srv.Generation = 4
	record(srv, cloudOf(srv), nil, transientErr(ReasonCloudError, "x"))
	if st.Discovered != nil {
		t.Errorf("transient after a spec change kept %v", st.Discovered)
	}
	st.Discovered = map[string]string{"endpoint": "h"}
	record(srv, cloudOf(srv), nil, errors.New("something else"))
	if c = meta.FindStatusCondition(st.Conditions, ConditionCloudResolved); c.Reason != ReasonCloudError || st.Discovered != nil {
		t.Errorf("plain error: %+v %v", c, st.Discovered)
	}
	record(srv, nil, nil, nil)
	if st.Discovered != nil || meta.FindStatusCondition(st.Conditions, ConditionCloudResolved) != nil {
		t.Errorf("self-hosted: %+v", st)
	}
	record(nil, nil, nil, nil) // an unknown kind is ignored
	if serverStatusOf(&dbv1alpha1.DatabaseServer{}) == nil || serverStatusOf(nil) != nil {
		t.Error("serverStatusOf")
	}
}

func TestProberDiscoversTheCloudAddress(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	ctx := context.Background()
	h.must(env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "aws", Namespace: h.ops},
		Data: map[string][]byte{"accessKeyId": []byte("AKIAX"), "secretAccessKey": []byte("s")}}))
	srv := awsServer("eu-west-1")
	srv.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "aws", Namespace: h.ops}}
	api := &fakeRDS{inst: instanceOut()}
	fake := &pgFake{answer: versionAnswer}
	cfg := h.cfg
	cfg.Connect, cfg.EgressMode = fake.connect, egress.ModeStrict
	cfg.RDS = func(aws.Config) RDSAPI { return api }
	p := Prober{Config: cfg}

	v, err := p.Probe(ctx, srv, probeConn())
	if err != nil || v == "" || len(fake.targets) != 1 || fake.targets[0].Host != "orders.abc.eu-west-1.rds.amazonaws.com" || fake.targets[0].Port != 5433 {
		t.Fatalf("probe: %q, %v, %+v", v, err, fake.targets)
	}
	if srv.Status.Discovered["resourceId"] != "db-ABC" {
		t.Errorf("discovered = %v", srv.Status.Discovered)
	}
	// An explicit host overrides the discovered address (an emulator).
	srv.Spec.Host, srv.Spec.Port = "10.0.0.7", 6432
	if _, err = p.Probe(ctx, srv, probeConn()); err != nil || fake.targets[1].Host != "10.0.0.7" || fake.targets[1].Port != 6432 {
		t.Errorf("override: %v, %+v", err, fake.targets)
	}
	// A target that is gone fails the probe with a category and no connection.
	api.inst, api.err = nil, &rdstypes.DBInstanceNotFoundFault{}
	if _, err = p.Probe(ctx, srv, probeConn()); reasonOf(t, err) != ReasonTargetNotFound || len(fake.targets) != 2 {
		t.Errorf("gone: %v, %d connections", err, len(fake.targets))
	}
}

// A cloud server's claims use the address the probe discovered, and a server
// the probe has not resolved cannot be used.
func TestCloudServerBackendAndBinding(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, func(s *dbv1alpha1.ClusterDatabaseServer) {
		s.Spec.Host, s.Spec.Port = "", 0
		p := awsServer("eu-west-1").Spec.Provider
		s.Spec.Provider = p
		s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret,
			SecretRef: &cloud.SecretRef{Name: "aws", Namespace: s.Spec.AdminCredentialsSecretRef.Namespace}}
	})
	ctx := context.Background()
	if _, err := h.cfg.backend(ctx, h.ns, h.ref()); !errors.Is(err, ErrNotDiscovered) || !errors.Is(err, server.ErrInvalidServer) {
		t.Fatalf("undiscovered: %v", err)
	}
	h.get(h.server)
	h.server.Status.Discovered = map[string]string{"endpoint": "orders.abc.rds.test", "port": "5433", "region": "eu-west-1"}
	h.must(env.Client.Status().Update(ctx, h.server))
	be, err := h.cfg.backend(ctx, h.ns, h.ref())
	if err != nil || be.host != "orders.abc.rds.test" || be.port != 5433 || !be.cloud || be.region != "eu-west-1" {
		t.Fatalf("backend: %+v, %v", be, err)
	}

	dbr, rr := &DatabaseReconciler{h.cfg}, &RoleReconciler{h.cfg}
	h.reconcileOK(dbr, h.database("orders", nil))
	role := h.role("orders-app", "orders", nil)
	h.reconcileOK(rr, role)
	data := bindingSecret(h, role)
	if data["host"] != "orders.abc.rds.test" || data["port"] != "5433" || data["cloud"] != "aws" ||
		data["authMode"] != "Password" || data["region"] != "eu-west-1" || data["password"] == "" {
		t.Errorf("binding = %v", data)
	}
}

type codedErr struct{ code string }

func (e codedErr) Error() string     { return "coded" }
func (e codedErr) ErrorCode() string { return e.code }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestRDSErrorClassification(t *testing.T) {
	transient := func(err error) bool {
		var ce *CloudError
		return errors.As(rdsErr(err), &ce) && ce.transient
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"throttled": {codedErr{"Throttling"}, true}, "limit": {codedErr{"RequestLimitExceeded"}, true},
		"denied": {codedErr{"AccessDenied"}, false}, "expired": {codedErr{"ExpiredToken"}, false},
		"deadline": {context.DeadlineExceeded, true}, "network": {timeoutErr{}, true}, "plain": {errors.New("x"), false},
	} {
		if transient(tc.err) != tc.want {
			t.Errorf("%s: transient = %v", name, !tc.want)
		}
	}
}
