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
	// DiscoveredRegion is the AWS region of the target.
	DiscoveredRegion = "region"
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

// Provider says the server is a managed cloud database. Absent: self-hosted.
// +kubebuilder:validation:XValidation:rule="self.type == 'AWS' ? has(self.aws) : !has(self.aws)",message="aws is required with type AWS and forbidden otherwise"
type Provider struct {
	// type is the cloud. Only AWS is supported in this version.
	Type cloud.ProviderType `json:"type"`
	// aws holds the AWS settings.
	// +optional
	AWS *AWSDatabase `json:"aws,omitempty"`
}

// ClusterProvider is the provider of a cluster server.
// +kubebuilder:validation:XValidation:rule="self.type == 'AWS' ? has(self.aws) : !has(self.aws)",message="aws is required with type AWS and forbidden otherwise"
type ClusterProvider struct {
	// type is the cloud. Only AWS is supported in this version.
	Type cloud.ProviderType `json:"type"`
	// aws holds the AWS settings.
	// +optional
	AWS *ClusterAWSDatabase `json:"aws,omitempty"`
}

// DatabaseServerStatus is the status of both server kinds: the common one and
// what the probe read from the cloud.
type DatabaseServerStatus struct {
	common.ServerStatus `json:",inline"`

	// discovered is what the probe read from the cloud: endpoint, port,
	// resourceId, accountId and region.
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
