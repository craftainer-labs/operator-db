package v1alpha1

import (
	"net"
	"strconv"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
)

// Keys of status.discovered: what the probe read from the cloud (ADR 0002,
// section 16.1).
const (
	// DiscoveredEndpoint is the data-plane host name.
	DiscoveredEndpoint = "endpoint"
	// DiscoveredPort is the data-plane TCP port.
	DiscoveredPort = "port"
	// DiscoveredResourceID is the RDS DbiResourceId or DbClusterResourceId.
	DiscoveredResourceID = "resourceId"
	// DiscoveredAccountID is the AWS account that owns the target.
	DiscoveredAccountID = "accountId"
	// DiscoveredRegion is the AWS region of the target, or the Cloud SQL
	// instance's or Azure server's region.
	DiscoveredRegion = "region"
	// DiscoveredInstanceConnectionName is the Cloud SQL connection name
	// (project:location:instance), as the Cloud SQL Admin API reports it.
	DiscoveredInstanceConnectionName = "instanceConnectionName"
	// DiscoveredDNSName is the Cloud SQL instance's DNS name, the host name its
	// server certificate carries when it has one.
	DiscoveredDNSName = "dnsName"
)

// AWSTarget names the RDS database a server stands for: exactly one of
// dbInstanceIdentifier and dbClusterIdentifier (checked by the block that
// embeds it).
type AWSTarget struct {
	// dbInstanceIdentifier is an RDS DB instance.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9-]*$`
	// +optional
	DBInstanceIdentifier string `json:"dbInstanceIdentifier,omitempty"`

	// dbClusterIdentifier is an Aurora DB cluster; its writer endpoint is used.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9-]*$`
	// +optional
	DBClusterIdentifier string `json:"dbClusterIdentifier,omitempty"`
}

// AWSDatabase is provider.aws of a namespaced server (no endpointOverride).
// +kubebuilder:validation:XValidation:rule="has(self.dbInstanceIdentifier) != has(self.dbClusterIdentifier)",message="set exactly one of dbInstanceIdentifier and dbClusterIdentifier"
type AWSDatabase struct {
	cloud.AWSBlock `json:",inline"`
	AWSTarget      `json:",inline"`
}

// ClusterAWSDatabase is provider.aws of a cluster server.
// +kubebuilder:validation:XValidation:rule="has(self.dbInstanceIdentifier) != has(self.dbClusterIdentifier)",message="set exactly one of dbInstanceIdentifier and dbClusterIdentifier"
type ClusterAWSDatabase struct {
	cloud.AWSClusterBlock `json:",inline"`
	AWSTarget             `json:",inline"`
}

// GCPTarget names the Cloud SQL instance a server stands for. With the
// block's project and location it makes the connection name
// project:location:instance.
type GCPTarget struct {
	// instance is the Cloud SQL instance name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=98
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Instance string `json:"instance"`
}

// GCPDatabase is provider.gcp of a namespaced server (no endpointOverride).
// Cloud SQL instances are regional, so location is required.
// +kubebuilder:validation:XValidation:rule="has(self.location) && size(self.location) > 0",message="location is required for a Cloud SQL instance"
type GCPDatabase struct {
	cloud.GCPBlock `json:",inline"`
	GCPTarget      `json:",inline"`
}

// ClusterGCPDatabase is provider.gcp of a cluster server.
// +kubebuilder:validation:XValidation:rule="has(self.location) && size(self.location) > 0",message="location is required for a Cloud SQL instance"
type ClusterGCPDatabase struct {
	cloud.GCPClusterBlock `json:",inline"`
	GCPTarget             `json:",inline"`
}

// AzureTarget names the Azure Database for PostgreSQL flexible server a server
// stands for, in the block's resource group.
type AzureTarget struct {
	// serverName is the flexible server's name.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ServerName string `json:"serverName"`
}

// AzureDatabase is provider.azure of a namespaced server (no endpointOverride).
// The subscription and the resource group are needed to address the server.
// +kubebuilder:validation:XValidation:rule="has(self.subscriptionID) && has(self.resourceGroup)",message="subscriptionID and resourceGroup are required for a flexible server"
// +kubebuilder:validation:XValidation:rule="!has(self.resourceGroup) || !self.resourceGroup.endsWith('.')",message="resourceGroup must not end with a period (this also rules out . and ..)"
type AzureDatabase struct {
	cloud.AzureBlock `json:",inline"`
	AzureTarget      `json:",inline"`
}

// ClusterAzureDatabase is provider.azure of a cluster server.
// +kubebuilder:validation:XValidation:rule="has(self.subscriptionID) && has(self.resourceGroup)",message="subscriptionID and resourceGroup are required for a flexible server"
// +kubebuilder:validation:XValidation:rule="!has(self.resourceGroup) || !self.resourceGroup.endsWith('.')",message="resourceGroup must not end with a period (this also rules out . and ..)"
type ClusterAzureDatabase struct {
	cloud.AzureClusterBlock `json:",inline"`
	AzureTarget             `json:",inline"`
}

// Provider says the server is a managed cloud database. Absent: self-hosted.
// +kubebuilder:validation:XValidation:rule="self.type == 'AWS' ? has(self.aws) : !has(self.aws)",message="aws is required with type AWS and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'GCP' ? has(self.gcp) : !has(self.gcp)",message="gcp is required with type GCP and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'Azure' ? has(self.azure) : !has(self.azure)",message="azure is required with type Azure and forbidden otherwise"
type Provider struct {
	// type is the cloud: AWS, GCP or Azure.
	Type cloud.ProviderType `json:"type"`
	// aws holds the AWS settings.
	// +optional
	AWS *AWSDatabase `json:"aws,omitempty"`
	// gcp holds the Google Cloud settings (a Cloud SQL instance).
	// +optional
	GCP *GCPDatabase `json:"gcp,omitempty"`
	// azure holds the Azure settings (a PostgreSQL flexible server).
	// +optional
	Azure *AzureDatabase `json:"azure,omitempty"`
}

// ClusterProvider is the provider of a cluster server.
// +kubebuilder:validation:XValidation:rule="self.type == 'AWS' ? has(self.aws) : !has(self.aws)",message="aws is required with type AWS and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'GCP' ? has(self.gcp) : !has(self.gcp)",message="gcp is required with type GCP and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'Azure' ? has(self.azure) : !has(self.azure)",message="azure is required with type Azure and forbidden otherwise"
type ClusterProvider struct {
	// type is the cloud: AWS, GCP or Azure.
	Type cloud.ProviderType `json:"type"`
	// aws holds the AWS settings.
	// +optional
	AWS *ClusterAWSDatabase `json:"aws,omitempty"`
	// gcp holds the Google Cloud settings (a Cloud SQL instance).
	// +optional
	GCP *ClusterGCPDatabase `json:"gcp,omitempty"`
	// azure holds the Azure settings (a PostgreSQL flexible server).
	// +optional
	Azure *ClusterAzureDatabase `json:"azure,omitempty"`
}

// DatabaseServerStatus is the status of both server kinds: the common one and
// what the probe read from the cloud.
type DatabaseServerStatus struct {
	common.ServerStatus `json:",inline"`

	// discovered is what the probe read from the cloud: endpoint, port,
	// resourceId, accountId and region on AWS; endpoint, port, region,
	// instanceConnectionName and dnsName on Cloud SQL; endpoint, port, resourceId
	// and region on an Azure flexible server.
	//
	// +kubebuilder:validation:MaxProperties=10
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 512)",message="values are at most 512 characters"
	// +optional
	Discovered map[string]string `json:"discovered,omitempty"`
}

// AuthMode is how a DatabaseRole's login is authenticated.
// +kubebuilder:validation:Enum=Password;IAM
type AuthMode string

// Values of AuthMode.
const (
	// AuthModePassword is a generated password in the binding Secret.
	AuthModePassword AuthMode = "Password"
	// AuthModeIAM is a short-lived cloud token; no password.
	AuthModeIAM AuthMode = "IAM"
)

// Target is the effective data-plane address of a server: spec.host and
// spec.port when host is set (self-hosted, or an override), else the
// discovered endpoint and port.
func Target(p PostgreSQL, discovered map[string]string) (host string, port int32) {
	if p.Host != "" {
		return p.Host, orDefault(p.Port)
	}
	port = orDefault(p.Port)
	if v, err := strconv.ParseInt(discovered[DiscoveredPort], 10, 32); err == nil && v >= 1 && v <= 65535 {
		port = int32(v)
	}
	return discovered[DiscoveredEndpoint], port
}

func orDefault(p int32) int32 {
	if p == 0 {
		return DefaultPort
	}
	return p
}

func hostPort(p PostgreSQL, discovered map[string]string) string {
	h, port := Target(p, discovered)
	return net.JoinHostPort(h, strconv.Itoa(int(port)))
}
