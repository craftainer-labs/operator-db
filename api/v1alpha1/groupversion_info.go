// Package v1alpha1 contains the API schema definitions of the
// db.craftainer.dev v1alpha1 group: the PostgreSQL servers, databases and
// roles that operator-db provisions. The shared blocks (claim, server and
// credential specs and statuses) are lib-operator-go's pkg/apis/common types,
// embedded here; docs/crd-drafts.md and ADR 0002 of lib-operator-go are their
// specification.
// +kubebuilder:object:generate=true
// +groupName=db.craftainer.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion is group version used to register these objects.
	// This name is used by applyconfiguration generators (e.g. controller-gen).
	SchemeGroupVersion = schema.GroupVersion{Group: "db.craftainer.dev", Version: "v1alpha1"}

	// GroupVersion is an alias for SchemeGroupVersion, for backward compatibility.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
