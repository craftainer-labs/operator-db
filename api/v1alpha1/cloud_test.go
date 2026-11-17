package v1alpha1

import (
	"strings"
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

func gcpCluster(name string) *ClusterDatabaseServer {
	s := clusterServerObj(name)
	s.Spec.Host = ""
	s.Spec.Provider = &ClusterProvider{Type: cloud.ProviderGCP, GCP: &ClusterGCPDatabase{
		GCPClusterBlock: cloud.GCPClusterBlock{GCPBlock: cloud.GCPBlock{Project: "proj-12345", Location: "europe-west1"}},
		GCPTarget:       GCPTarget{Instance: "orders"},
	}}
	return s
}

const (
	testTenant = "00000000-0000-4000-8000-000000000001"
	testSub    = "00000000-0000-4000-8000-000000000002"
)

func azureCluster(name string) *ClusterDatabaseServer {
	s := clusterServerObj(name)
	s.Spec.Host = ""
	s.Spec.Provider = &ClusterProvider{Type: cloud.ProviderAzure, Azure: &ClusterAzureDatabase{
		AzureClusterBlock: cloud.AzureClusterBlock{AzureBlock: cloud.AzureBlock{TenantID: testTenant, SubscriptionID: testSub, ResourceGroup: "rg-orders"}},
		AzureTarget:       AzureTarget{ServerName: "orders"},
	}}
	return s
}

func azureNamespaced(name string) *DatabaseServer {
	s := serverObj(name)
	s.Spec.Host = ""
	s.Spec.Provider = &Provider{Type: cloud.ProviderAzure, Azure: &AzureDatabase{
		AzureBlock:  cloud.AzureBlock{TenantID: testTenant, SubscriptionID: testSub, ResourceGroup: "rg-orders"},
		AzureTarget: AzureTarget{ServerName: "orders"}}}
	s.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "azure"}}
	return s
}

// An Azure flexible server: the provider.azure block, its target, and the rules
// it shares with the other clouds.
func TestAzureProviderRules(t *testing.T) {
	g := NewWithT(t)
	g.Expect(k8sClient.Create(ctx, azureCluster("az-ok"))).To(Succeed())
	secret := azureCluster("az-secret")
	secret.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "azure", Namespace: "operator-db-system"}}
	secret.Spec.Provider.Azure.EndpointOverride = "https://login.example.test:8443"
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	g.Expect(k8sClient.Create(ctx, azureNamespaced("az-ns"))).To(Succeed())

	noBlock := azureCluster("az-no-block")
	noBlock.Spec.Provider.Azure = nil
	invalid(t, noBlock, "azure is required with type Azure")
	wrongType := azureCluster("az-wrong-type")
	wrongType.Spec.Provider.Type = cloud.ProviderGCP
	invalid(t, wrongType, "gcp is required with type GCP")
	awsTypeAzureBlock := azureCluster("aws-type-azure-block")
	awsTypeAzureBlock.Spec.Provider.Type = cloud.ProviderAWS
	invalid(t, awsTypeAzureBlock, "aws is required with type AWS")
	both := gcpCluster("az-and-gcp")
	both.Spec.Provider.Azure = azureCluster("x").Spec.Provider.Azure
	invalid(t, both, "azure is required with type Azure and forbidden otherwise")
	for name, mutate := range map[string]func(*ClusterAzureDatabase){
		"no-sub":        func(a *ClusterAzureDatabase) { a.SubscriptionID = "" },
		"no-group":      func(a *ClusterAzureDatabase) { a.ResourceGroup = "" },
		"bad-sub":       func(a *ClusterAzureDatabase) { a.SubscriptionID = "not-a-guid" },
		"no-tenant":     func(a *ClusterAzureDatabase) { a.TenantID = "" },
		"no-server":     func(a *ClusterAzureDatabase) { a.ServerName = "" },
		"short-server":  func(a *ClusterAzureDatabase) { a.ServerName = "ab" },
		"upper-server":  func(a *ClusterAzureDatabase) { a.ServerName = "Orders" },
		"hyphen-server": func(a *ClusterAzureDatabase) { a.ServerName = "orders-" },
	} {
		s := azureCluster("az-" + name)
		mutate(s.Spec.Provider.Azure)
		invalid(t, s, "")
	}
	noGroupNS := azureNamespaced("az-ns-no-group")
	noGroupNS.Spec.Provider.Azure.ResourceGroup = ""
	invalid(t, noGroupNS, "subscriptionID and resourceGroup are required")
	noSub := azureCluster("az-no-sub-msg")
	noSub.Spec.Provider.Azure.SubscriptionID = ""
	invalid(t, noSub, "subscriptionID and resourceGroup are required")
	// A namespaced server cannot use WorkloadIdentity.
	wi := azureNamespaced("az-ns-wi")
	wi.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceWorkloadIdentity}
	invalid(t, wi, "must use source Secret")
}

func gcpNamespaced(name string) *DatabaseServer {
	s := serverObj(name)
	s.Spec.Host = ""
	s.Spec.Provider = &Provider{Type: cloud.ProviderGCP, GCP: &GCPDatabase{
		GCPBlock: cloud.GCPBlock{Project: "proj-12345", Location: "europe-west1"}, GCPTarget: GCPTarget{Instance: "orders"}}}
	s.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "gcp"}}
	return s
}

// A Cloud SQL server: the provider.gcp block, its target and location, and the
// rules it shares with AWS.
func TestGCPProviderRules(t *testing.T) {
	g := NewWithT(t)
	g.Expect(k8sClient.Create(ctx, gcpCluster("gcp-ok"))).To(Succeed())
	secret := gcpCluster("gcp-secret")
	secret.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "gcp", Namespace: "operator-db-system"}}
	secret.Spec.Provider.GCP.EndpointOverride = "http://sqladmin.example.test:8080"
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	g.Expect(k8sClient.Create(ctx, gcpNamespaced("gcp-ns"))).To(Succeed())

	noBlock := gcpCluster("gcp-no-block")
	noBlock.Spec.Provider.GCP = nil
	invalid(t, noBlock, "gcp is required with type GCP")
	noLocation := gcpCluster("gcp-no-location")
	noLocation.Spec.Provider.GCP.Location = ""
	invalid(t, noLocation, "location is required for a Cloud SQL instance")
	noLocationNS := gcpNamespaced("gcp-no-location-ns")
	noLocationNS.Spec.Provider.GCP.Location = ""
	invalid(t, noLocationNS, "location is required for a Cloud SQL instance")
	noInstance := gcpCluster("gcp-no-instance")
	noInstance.Spec.Provider.GCP.Instance = ""
	invalid(t, noInstance, "instance")
	badInstance := gcpCluster("gcp-bad-instance")
	badInstance.Spec.Provider.GCP.Instance = "Orders_1"
	invalid(t, badInstance, "instance")
	badProject := gcpCluster("gcp-bad-project")
	badProject.Spec.Provider.GCP.Project = "X"
	invalid(t, badProject, "project")
	// A namespaced server cannot use WorkloadIdentity.
	wi := gcpNamespaced("gcp-ns-wi")
	wi.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceWorkloadIdentity}
	invalid(t, wi, "must use source Secret")
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
	azure := awsCluster("azure")
	azure.Spec.Provider.Type = cloud.ProviderAzure
	invalid(t, azure, "azure is required with type Azure")
	azureNS := awsNamespaced("azure-ns")
	azureNS.Spec.Provider.Type = cloud.ProviderAzure
	invalid(t, azureNS, "azure is required with type Azure")
	// The block must be the one of the type.
	gcpTypeAWSBlock := awsCluster("gcp-type-aws-block")
	gcpTypeAWSBlock.Spec.Provider.Type = cloud.ProviderGCP
	invalid(t, gcpTypeAWSBlock, "gcp is required with type GCP")
	awsTypeGCPBlock := gcpCluster("aws-type-gcp-block")
	awsTypeGCPBlock.Spec.Provider.Type = cloud.ProviderAWS
	invalid(t, awsTypeGCPBlock, "aws is required with type AWS")
	both2 := gcpCluster("both-blocks")
	both2.Spec.Provider.AWS = awsCluster("x").Spec.Provider.AWS
	invalid(t, both2, "aws is required with type AWS and forbidden otherwise")
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

// iam.principal: only with authMode IAM, bounded, and immutable.
func TestIAMPrincipalRules(t *testing.T) {
	g := NewWithT(t)
	ok := roleObj("principal-ok")
	ok.Spec.AuthMode = AuthModeIAM
	ok.Spec.IAM = &IAMSpec{Principal: "app@proj-12345.iam"}
	g.Expect(k8sClient.Create(ctx, ok)).To(Succeed())
	ok.Spec.IAM.Principal = "other@proj-12345.iam"
	g.Expect(k8sClient.Update(ctx, ok)).To(MatchError(ContainSubstring("iam.principal is immutable")))

	password := roleObj("principal-password")
	password.Spec.IAM = &IAMSpec{Principal: "app@proj-12345.iam"}
	invalid(t, password, "iam is allowed only with authMode IAM")
	empty := roleObj("principal-empty")
	empty.Spec.AuthMode = AuthModeIAM
	empty.Spec.IAM = &IAMSpec{}
	invalid(t, empty, "principal")
	long := roleObj("principal-long")
	long.Spec.AuthMode = AuthModeIAM
	long.Spec.IAM = &IAMSpec{Principal: strings.Repeat("a", 321)}
	invalid(t, long, "principal")
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
