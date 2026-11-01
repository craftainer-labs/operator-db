package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// Privileges is the access a DatabaseRole has to its database.
// +kubebuilder:validation:Enum=ReadOnly;ReadWrite;Owner
type Privileges string

// Values of Privileges.
const (
	// PrivilegesReadOnly can read.
	PrivilegesReadOnly Privileges = "ReadOnly"
	// PrivilegesReadWrite can read and write data.
	PrivilegesReadWrite Privileges = "ReadWrite"
	// PrivilegesOwner owns the database's objects and can change its schema.
	PrivilegesOwner Privileges = "Owner"
)

// DatabaseRef names a Database in the claim's own namespace.
type DatabaseRef struct {
	// name is the Database's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="databaseRef.name is immutable"
	Name string `json:"name"`
}

// DatabaseRoleSpec defines the desired state of DatabaseRole. It is a
// credential kind: it has no deletionPolicy (the role is always deleted with
// the claim) and its login is written to a binding Secret.
// +kubebuilder:validation:XValidation:rule="self.serverRef.kind in ['ClusterDatabaseServer', 'DatabaseServer']",message="serverRef.kind must be ClusterDatabaseServer or DatabaseServer"
// +kubebuilder:validation:XValidation:rule="!has(self.rotationPolicy) || self.managementPolicies == ['*'] || 'Update' in self.managementPolicies",message="rotationPolicy needs the Update management policy"
type DatabaseRoleSpec struct {
	common.ClaimCommon `json:",inline"`
	common.Credentials `json:",inline"`

	// databaseRef names the Database, in the same namespace, the role is for.
	DatabaseRef DatabaseRef `json:"databaseRef"`

	// privileges is the access the role has to the database.
	// +kubebuilder:default=ReadWrite
	// +optional
	Privileges Privileges `json:"privileges,omitempty"`

	// connectionLimit caps concurrent connections of the role; -1 means
	// unlimited.
	// +kubebuilder:validation:Minimum=-1
	// +kubebuilder:default=-1
	// +optional
	ConnectionLimit *int32 `json:"connectionLimit,omitempty"`
}

// DatabaseRoleStatus defines the observed state of DatabaseRole. externalName
// is the base role; the logins are suffixed _a and _b.
type DatabaseRoleStatus struct {
	common.ClaimStatus       `json:",inline"`
	common.CredentialsStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DatabaseRole is a PostgreSQL login with access to one Database. Its
// credentials are written to a binding Secret of type servicebinding.io/postgresql.
type DatabaseRole struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DatabaseRole
	// +required
	Spec DatabaseRoleSpec `json:"spec"`

	// status defines the observed state of DatabaseRole
	// +optional
	Status DatabaseRoleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DatabaseRoleList contains a list of DatabaseRole.
type DatabaseRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DatabaseRole `json:"items"`
}

// GetServerRef returns spec.serverRef (lifecycle.Claim).
func (r *DatabaseRole) GetServerRef() common.ServerRef { return r.Spec.ServerRef }

// GetDeletionPolicy returns "": a credential kind has no deletionPolicy.
func (r *DatabaseRole) GetDeletionPolicy() common.DeletionPolicy { return "" }

// GetManagementPolicies returns spec.managementPolicies.
func (r *DatabaseRole) GetManagementPolicies() []common.ManagementPolicy {
	return r.Spec.ManagementPolicies
}

// GetConditions returns a pointer to status.conditions.
func (r *DatabaseRole) GetConditions() *[]metav1.Condition { return &r.Status.Conditions }

// GetBindingSecretName returns spec.binding.secretName, or the claim's name
// when it is unset.
func (r *DatabaseRole) GetBindingSecretName() string {
	if r.Spec.Binding.SecretName != "" {
		return r.Spec.Binding.SecretName
	}
	return r.Name
}

// SetBindingName sets status.binding.name.
func (r *DatabaseRole) SetBindingName(n string) { r.Status.Binding = &common.BindingStatus{Name: n} }

// GetExternalName returns status.externalName.
func (r *DatabaseRole) GetExternalName() string { return r.Status.ExternalName }

// SetExternalName sets status.externalName.
func (r *DatabaseRole) SetExternalName(n string) { r.Status.ExternalName = n }

// GetExternalID returns status.externalId.
func (r *DatabaseRole) GetExternalID() string { return r.Status.ExternalID }

// SetExternalID sets status.externalId.
func (r *DatabaseRole) SetExternalID(id string) { r.Status.ExternalID = id }

// SetObservedGeneration sets status.observedGeneration.
func (r *DatabaseRole) SetObservedGeneration(g int64) { r.Status.ObservedGeneration = g }

// SetLastReconcileTime sets status.lastReconcileTime.
func (r *DatabaseRole) SetLastReconcileTime(t metav1.Time) { r.Status.LastReconcileTime = &t }

// GetEffectiveDeletionPolicy returns "": a credential kind has none.
func (r *DatabaseRole) GetEffectiveDeletionPolicy() common.DeletionPolicy { return "" }

// SetEffectiveDeletionPolicy does nothing: a credential kind has no deletion policy.
func (r *DatabaseRole) SetEffectiveDeletionPolicy(common.DeletionPolicy, common.DeletionPolicySource) {
}

// SetLateInitialized sets status.lateInitialized.
func (r *DatabaseRole) SetLateInitialized(m map[string]string) { r.Status.LateInitialized = m }

// GetRotationPolicy returns spec.rotationPolicy; nil when unset.
func (r *DatabaseRole) GetRotationPolicy() *common.RotationPolicy { return r.Spec.RotationPolicy }

// GetRotationStatus returns status.rotation, the zero value when unset.
func (r *DatabaseRole) GetRotationStatus() common.RotationStatus {
	if r.Status.Rotation == nil {
		return common.RotationStatus{}
	}
	return *r.Status.Rotation
}

// SetRotationStatus sets status.rotation.
func (r *DatabaseRole) SetRotationStatus(s common.RotationStatus) { r.Status.Rotation = &s }

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DatabaseRole{}, &DatabaseRoleList{})
		return nil
	})
}
