package v1alpha1

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
)

func rotationPolicy() *common.RotationPolicy {
	d := metav1.Duration{Duration: 24 * time.Hour}
	return &common.RotationPolicy{Interval: &d}
}

func awsCluster(name string) *ClusterDatabaseServer {
	s := clusterServerObj(name)
	s.Spec.Host = ""
	s.Spec.Provider = &ClusterProvider{Type: cloud.ProviderAWS, AWS: &ClusterAWSDatabase{
		AWSClusterBlock: cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: "eu-west-1"}},
		AWSTarget:       AWSTarget{DBInstanceIdentifier: "orders"},
	}}
	return s
}

func awsNamespaced(name string) *DatabaseServer {
	s := serverObj(name)
	s.Spec.Host = ""
	s.Spec.Provider = &Provider{Type: cloud.ProviderAWS, AWS: &AWSDatabase{
		AWSBlock: cloud.AWSBlock{Region: "eu-west-1"}, AWSTarget: AWSTarget{DBClusterIdentifier: "aurora"}}}
	s.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "aws"}}
	return s
}

func TestProviderRules(t *testing.T) {
	g := NewWithT(t)

	// A cluster server defaults to WorkloadIdentity: credentials may be absent.
	ok := awsCluster("aws-ok")
	g.Expect(k8sClient.Create(ctx, ok)).To(Succeed())
	secret := awsCluster("aws-secret")
	secret.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "aws", Namespace: "operator-db-system"}}
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	g.Expect(k8sClient.Create(ctx, awsNamespaced("aws-ns"))).To(Succeed())
	// A provider drops the host, and still takes one as an override.
	over := awsCluster("aws-override")
	over.Spec.Host = "10.0.0.7"
	g.Expect(k8sClient.Create(ctx, over)).To(Succeed())

	noHost := clusterServerObj("no-host")
	noHost.Spec.Host = ""
	invalid(t, noHost, "host is required without provider")
	noHostNS := serverObj("no-host")
	noHostNS.Spec.Host = ""
	invalid(t, noHostNS, "host is required without provider")

	// credentials only with a provider.
	lone := clusterServerObj("lone-creds")
	lone.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceWorkloadIdentity}
	invalid(t, lone, "credentials is allowed only with provider")

	// The provider union and the target.
	noBlock := awsCluster("no-block")
	noBlock.Spec.Provider.AWS = nil
	invalid(t, noBlock, "aws is required with type AWS")
	gcp := awsCluster("gcp")
	gcp.Spec.Provider.Type = cloud.ProviderGCP
	invalid(t, gcp, "only provider.type AWS is supported")
	none := awsCluster("no-target")
	none.Spec.Provider.AWS.DBInstanceIdentifier = ""
	invalid(t, none, "exactly one of dbInstanceIdentifier and dbClusterIdentifier")
	both := awsCluster("both-targets")
	both.Spec.Provider.AWS.DBClusterIdentifier = "x"
	invalid(t, both, "exactly one of dbInstanceIdentifier and dbClusterIdentifier")
	region := awsCluster("bad-region")
	region.Spec.Provider.AWS.Region = "mars"
	invalid(t, region, "region")

	// Credentials: Secret needs its ref, WorkloadIdentity forbids it, a cluster
	// server's ref needs a namespace.
	noRef := awsCluster("secret-no-ref")
	noRef.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	invalid(t, noRef, "secretRef is required when source is Secret")
	noRefNS := awsCluster("secret-no-ns")
	noRefNS.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "aws"}}
	invalid(t, noRefNS, "secretRef.namespace is required")

	// A namespaced server cannot use WorkloadIdentity, set a namespace, or
	// omit credentials.
	wi := awsNamespaced("ns-wi")
	wi.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceWorkloadIdentity}
	invalid(t, wi, "must use source Secret")
	nsNS := awsNamespaced("ns-ns")
	nsNS.Spec.Credentials.SecretRef.Namespace = "other"
	invalid(t, nsNS, "secretRef.namespace is forbidden")
	noCreds := awsNamespaced("ns-no-creds")
	noCreds.Spec.Credentials = nil
	invalid(t, noCreds, "must set credentials")

	// provider.type is immutable.
	ok.Spec.Provider.Type = cloud.ProviderAzure
	g.Expect(k8sClient.Update(ctx, ok)).To(MatchError(ContainSubstring("provider.type is immutable")))
}

func TestStatusDiscoveredAndAuthMode(t *testing.T) {
	g := NewWithT(t)
	s := awsCluster("discovered")
	g.Expect(k8sClient.Create(ctx, s)).To(Succeed())
	s.Status.Discovered = map[string]string{DiscoveredEndpoint: "h", DiscoveredPort: "5432"}
	g.Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
	too := map[string]string{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"} {
		too[k] = "v"
	}
	s.Status.Discovered = too
	g.Expect(k8sClient.Status().Update(ctx, s)).NotTo(Succeed())

	r := roleObj("password-default")
	g.Expect(k8sClient.Create(ctx, r)).To(Succeed())
	g.Expect(r.Spec.AuthMode).To(Equal(AuthModePassword))
	r.Spec.AuthMode = AuthModeIAM
	g.Expect(k8sClient.Update(ctx, r)).To(MatchError(ContainSubstring("authMode is immutable")))
	bad := roleObj("bad-mode")
	bad.Spec.AuthMode = "Token"
	invalid(t, bad, "authMode")
	iam := roleObj("iam-rotation")
	iam.Spec.AuthMode = AuthModeIAM
	iam.Spec.RotationPolicy = rotationPolicy()
	invalid(t, iam, "rotationPolicy is not allowed with authMode IAM")
}

func TestTarget(t *testing.T) {
	g := NewWithT(t)
	disc := map[string]string{DiscoveredEndpoint: "rds.test", DiscoveredPort: "5433"}
	host, port := Target(PostgreSQL{Host: "pg", Port: 6432}, disc)
	g.Expect(host).To(Equal("pg"))
	g.Expect(port).To(Equal(int32(6432)))
	host, port = Target(PostgreSQL{Host: "pg"}, disc)
	g.Expect(port).To(Equal(DefaultPort))
	g.Expect(host).To(Equal("pg"))
	host, port = Target(PostgreSQL{Port: 5432}, disc)
	g.Expect(host).To(Equal("rds.test"))
	g.Expect(port).To(Equal(int32(5433)))
	// A discovered port that is not one falls back to the spec's.
	for _, bad := range []string{"", "x", "0", "70000"} {
		_, port = Target(PostgreSQL{Port: 6432}, map[string]string{DiscoveredEndpoint: "h", DiscoveredPort: bad})
		g.Expect(port).To(Equal(int32(6432)), bad)
	}
	host, _ = Target(PostgreSQL{}, nil)
	g.Expect(host).To(BeEmpty())

	s := &ClusterDatabaseServer{}
	s.Status.Discovered = disc
	g.Expect(s.GetEndpoint()).To(Equal("rds.test:5433"))
}
