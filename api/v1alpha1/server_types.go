package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
)

// Kinds of the group's two server tiers, the values of spec.serverRef.kind.
const (
	// KindClusterDatabaseServer is the cluster-scoped server kind.
	KindClusterDatabaseServer = "ClusterDatabaseServer"
	// KindDatabaseServer is the namespaced server kind.
	KindDatabaseServer = "DatabaseServer"
)

// DefaultPort is PostgreSQL's port, the schema default of spec.port.
const DefaultPort int32 = 5432

// SSLMode is the libpq sslmode used to reach the server. The weaker modes
// (disable, allow, prefer) are not offered: TLS to every backend is required.
// +kubebuilder:validation:Enum=verify-full;verify-ca;require
type SSLMode string

// Values of SSLMode.
const (
	// SSLModeVerifyFull verifies the certificate chain and the host name.
	SSLModeVerifyFull SSLMode = "verify-full"
	// SSLModeVerifyCA is verified as verify-full: the operator never skips
	// the host name check.
	SSLModeVerifyCA SSLMode = "verify-ca"
	// SSLModeRequire is verified as verify-full: the operator never skips
	// certificate verification.
	SSLModeRequire SSLMode = "require"
)

// PostgreSQL holds the fields both server kinds add to common.ServerCommon.
type PostgreSQL struct {
	// host is the server's host name or IP address. It is required without
	// spec.provider; with one it overrides the discovered address (for an
	// emulator whose API and data plane are different processes).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Host string `json:"host,omitempty"`

	// port is the server's TCP port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// adminDatabase is the database the admin connection opens.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:default=postgres
	// +optional
	AdminDatabase string `json:"adminDatabase,omitempty"`

	// sslMode is the libpq sslmode. verify-full, verify-ca and require are
	// offered; disable, allow and prefer are not. The operator verifies the
	// certificate chain and the host name in every mode; set tls.serverName when
	// the certificate names another host.
	// +kubebuilder:default=verify-full
	// +optional
	SSLMode SSLMode `json:"sslMode,omitempty"`

	// allowedExtensions limits the extensions a Database may create;
	// spec.extensions of a Database must be a subset.
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z_][a-z0-9_-]*$`
	// +kubebuilder:default={}
	// +optional
	AllowedExtensions []string `json:"allowedExtensions,omitempty"`
}

// ClusterDatabaseServerSpec defines the desired state of ClusterDatabaseServer.
// +kubebuilder:validation:XValidation:rule="has(self.adminCredentialsSecretRef.namespace) && size(self.adminCredentialsSecretRef.namespace) > 0",message="adminCredentialsSecretRef.namespace is required on a cluster server: name the operator's namespace"
// +kubebuilder:validation:XValidation:rule="has(self.provider) || (has(self.host) && size(self.host) > 0)",message="host is required without provider"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider.type in ['AWS', 'GCP']",message="only provider.type AWS and GCP are supported"
// +kubebuilder:validation:XValidation:rule="!has(self.credentials) || has(self.provider)",message="credentials is allowed only with provider"
type ClusterDatabaseServerSpec struct {
	common.ServerCommon `json:",inline"`
	PostgreSQL          `json:",inline"`

	// provider says the server is a managed cloud database. It is immutable
	// in its type: claims already point at objects in the cloud it names.
	// +optional
	Provider *ClusterProvider `json:"provider,omitempty"`

	// credentials are the operator's credentials to the cloud API. Absent
	// with a provider: WorkloadIdentity.
	// +optional
	Credentials *cloud.Credentials `json:"credentials,omitempty"`

	// allowedNamespaces says which namespaces may use the server. It is
	// required, so a cluster server without it is rejected: it fails closed.
	AllowedNamespaces common.AllowedNamespaces `json:"allowedNamespaces"`
}

// DatabaseServerSpec defines the desired state of DatabaseServer. A namespaced
// server serves only claims in its own namespace and has no allowedNamespaces.
// +kubebuilder:validation:XValidation:rule="!has(self.adminCredentialsSecretRef.namespace)",message="adminCredentialsSecretRef.namespace is forbidden on a namespaced server: the Secret is in the server's namespace"
// +kubebuilder:validation:XValidation:rule="has(self.provider) || (has(self.host) && size(self.host) > 0)",message="host is required without provider"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider.type in ['AWS', 'GCP']",message="only provider.type AWS and GCP are supported"
// +kubebuilder:validation:XValidation:rule="!has(self.credentials) || has(self.provider)",message="credentials is allowed only with provider"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || has(self.credentials)",message="a namespaced server with a provider must set credentials with source Secret: it cannot use WorkloadIdentity"
type DatabaseServerSpec struct {
	common.ServerCommon `json:",inline"`
	PostgreSQL          `json:",inline"`

	// provider says the server is a managed cloud database.
	// +optional
	Provider *Provider `json:"provider,omitempty"`

	// credentials are the credentials to the cloud API: a Secret in the
	// server's namespace (a namespaced server cannot use WorkloadIdentity).
	// +optional
	Credentials *cloud.NamespacedCredentials `json:"credentials,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterDatabaseServer is a cluster-scoped PostgreSQL server that claims in
// the namespaces allowedNamespaces admits can use.
type ClusterDatabaseServer struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterDatabaseServer
	// +required
	Spec ClusterDatabaseServerSpec `json:"spec"`

	// status defines the observed state of ClusterDatabaseServer
	// +optional
	Status DatabaseServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterDatabaseServerList contains a list of ClusterDatabaseServer.
type ClusterDatabaseServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterDatabaseServer `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DatabaseServer is a namespaced PostgreSQL server for a team that brings its
// own: it serves only claims in its own namespace.
type DatabaseServer struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DatabaseServer
	// +required
	Spec DatabaseServerSpec `json:"spec"`

	// status defines the observed state of DatabaseServer
	// +optional
	Status DatabaseServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DatabaseServerList contains a list of DatabaseServer.
type DatabaseServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DatabaseServer `json:"items"`
}

// GetCommon returns the settings every server kind shares (server.Server).
func (s *ClusterDatabaseServer) GetCommon() *common.ServerCommon { return &s.Spec.ServerCommon }

// GetServerStatus returns the status the probe writes (probe.Server).
func (s *ClusterDatabaseServer) GetServerStatus() *common.ServerStatus { return &s.Status.ServerStatus }

// GetEndpoint returns host:port (server.Server).
func (s *ClusterDatabaseServer) GetEndpoint() string {
	return hostPort(s.Spec.PostgreSQL, s.Status.Discovered)
}

// GetAllowedNamespaces returns spec.allowedNamespaces (server.Server).
func (s *ClusterDatabaseServer) GetAllowedNamespaces() *common.AllowedNamespaces {
	return &s.Spec.AllowedNamespaces
}

// GetCommon returns the settings every server kind shares (server.Server).
func (s *DatabaseServer) GetCommon() *common.ServerCommon { return &s.Spec.ServerCommon }

// GetServerStatus returns the status the probe writes (probe.Server).
func (s *DatabaseServer) GetServerStatus() *common.ServerStatus { return &s.Status.ServerStatus }

// GetEndpoint returns host:port (server.Server).
func (s *DatabaseServer) GetEndpoint() string {
	return hostPort(s.Spec.PostgreSQL, s.Status.Discovered)
}

// GetAllowedNamespaces returns nil: a namespaced server has none (server.Server).
func (s *DatabaseServer) GetAllowedNamespaces() *common.AllowedNamespaces { return nil }

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion,
			&ClusterDatabaseServer{}, &ClusterDatabaseServerList{},
			&DatabaseServer{}, &DatabaseServerList{})
		return nil
	})
}
