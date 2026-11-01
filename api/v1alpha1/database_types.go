package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// DatabaseSpec defines the desired state of Database. It is a data kind: it
// carries deletionPolicy and has no binding Secret (access comes from a
// DatabaseRole).
// +kubebuilder:validation:XValidation:rule="self.serverRef.kind in ['ClusterDatabaseServer', 'DatabaseServer']",message="serverRef.kind must be ClusterDatabaseServer or DatabaseServer"
type DatabaseSpec struct {
	common.ClaimCommon `json:",inline"`
	common.DataClaim   `json:",inline"`

	// encoding is the database encoding. It is immutable.
	// +kubebuilder:validation:Enum=UTF8
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="encoding is immutable"
	// +kubebuilder:default=UTF8
	// +optional
	Encoding string `json:"encoding,omitempty"`

	// locale is the database locale; unset takes the server default. It is
	// immutable.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.@-]{1,64}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="locale is immutable"
	// +optional
	Locale string `json:"locale,omitempty"`

	// extensions are created in the database. Each must be in the server's
	// allowedExtensions; the controller checks that.
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MaxLength=63
	// +listType=set
	// +optional
	Extensions []string `json:"extensions,omitempty"`

	// connectionLimit caps concurrent connections to the database; -1 means
	// unlimited.
	// +kubebuilder:validation:Minimum=-1
	// +kubebuilder:default=-1
	// +optional
	ConnectionLimit *int32 `json:"connectionLimit,omitempty"`
}

// DatabaseStatus defines the observed state of Database. externalName is the
// PostgreSQL database's name.
type DatabaseStatus struct {
	common.ClaimStatus     `json:",inline"`
	common.DataClaimStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([a-z0-9-]*[a-z0-9])?$')",message="metadata.name must be lower-case alphanumerics and '-', with no dots"

// Database is a PostgreSQL database on a ClusterDatabaseServer or a
// DatabaseServer.
type Database struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Database
	// +required
	Spec DatabaseSpec `json:"spec"`

	// status defines the observed state of Database
	// +optional
	Status DatabaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DatabaseList contains a list of Database.
type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Database `json:"items"`
}

// GetServerRef returns spec.serverRef (lifecycle.Claim).
func (d *Database) GetServerRef() common.ServerRef { return d.Spec.ServerRef }

// GetDeletionPolicy returns spec.deletionPolicy; empty when unset.
func (d *Database) GetDeletionPolicy() common.DeletionPolicy { return d.Spec.DeletionPolicy }

// GetManagementPolicies returns spec.managementPolicies.
func (d *Database) GetManagementPolicies() []common.ManagementPolicy {
	return d.Spec.ManagementPolicies
}

// GetConditions returns a pointer to status.conditions.
func (d *Database) GetConditions() *[]metav1.Condition { return &d.Status.Conditions }

// GetBindingSecretName returns "": a data kind has no binding Secret.
func (d *Database) GetBindingSecretName() string { return "" }

// SetBindingName does nothing: a data kind has no binding.
func (d *Database) SetBindingName(string) {}

// GetExternalName returns status.externalName.
func (d *Database) GetExternalName() string { return d.Status.ExternalName }

// SetExternalName sets status.externalName.
func (d *Database) SetExternalName(n string) { d.Status.ExternalName = n }

// GetExternalID returns status.externalId.
func (d *Database) GetExternalID() string { return d.Status.ExternalID }

// SetExternalID sets status.externalId.
func (d *Database) SetExternalID(id string) { d.Status.ExternalID = id }

// SetObservedGeneration sets status.observedGeneration.
func (d *Database) SetObservedGeneration(g int64) { d.Status.ObservedGeneration = g }

// SetLastReconcileTime sets status.lastReconcileTime.
func (d *Database) SetLastReconcileTime(t metav1.Time) { d.Status.LastReconcileTime = &t }

// GetEffectiveDeletionPolicy returns the default pinned at the first reconcile.
func (d *Database) GetEffectiveDeletionPolicy() common.DeletionPolicy {
	return d.Status.EffectiveDeletionPolicy
}

// SetEffectiveDeletionPolicy pins the default with its source.
func (d *Database) SetEffectiveDeletionPolicy(p common.DeletionPolicy, src common.DeletionPolicySource) {
	d.Status.EffectiveDeletionPolicy = p
	d.Status.DeletionPolicySource = src
}

// SetLateInitialized sets status.lateInitialized.
func (d *Database) SetLateInitialized(m map[string]string) { d.Status.LateInitialized = m }

// GetRotationPolicy returns nil: a data kind has no rotationPolicy.
func (d *Database) GetRotationPolicy() *common.RotationPolicy { return nil }

// GetRotationStatus returns the zero value: a data kind does not rotate.
func (d *Database) GetRotationStatus() common.RotationStatus { return common.RotationStatus{} }

// SetRotationStatus does nothing: a data kind does not rotate.
func (d *Database) SetRotationStatus(common.RotationStatus) {}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Database{}, &DatabaseList{})
		return nil
	})
}
