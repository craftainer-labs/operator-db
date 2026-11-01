// Package v1alpha1 holds the admission webhooks of the db.craftainer.dev API.
// Defaults and most validation live in the CRD schema (OpenAPI and CEL), so
// the webhooks only check what the schema cannot express: that a server's
// host is a host name or an IP address and that its proxyURL is a usable
// http, https or socks5 URL. The serving certificate comes from cert-manager
// and a ValidatingWebhookConfiguration carries the CA it injects.
package v1alpha1

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
)

var serverlog = logf.Log.WithName("server-resource")

var (
	// ErrInvalidHost is returned for a spec.host that is neither a DNS-1123
	// host name nor an IP address.
	ErrInvalidHost = errors.New("spec.host must be a host name or an IP address")
	// ErrInvalidProxyURL is returned for a spec.proxyURL that does not parse
	// or is not an http, https or socks5 URL with a host.
	ErrInvalidProxyURL = errors.New("spec.proxyURL must be an http, https or socks5 URL with a host")
)

// SetupClusterDatabaseServerWebhookWithManager registers the
// ClusterDatabaseServer webhook with the manager.
func SetupClusterDatabaseServerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &dbv1alpha1.ClusterDatabaseServer{}).
		WithValidator(&ClusterDatabaseServerValidator{}).
		Complete()
}

// SetupDatabaseServerWebhookWithManager registers the DatabaseServer webhook
// with the manager.
func SetupDatabaseServerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &dbv1alpha1.DatabaseServer{}).
		WithValidator(&DatabaseServerValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-db-craftainer-dev-v1alpha1-clusterdatabaseserver,mutating=false,failurePolicy=fail,sideEffects=None,groups=db.craftainer.dev,resources=clusterdatabaseservers,verbs=create;update,versions=v1alpha1,name=vclusterdatabaseserver-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterDatabaseServerValidator validates ClusterDatabaseServers on create
// and update.
type ClusterDatabaseServerValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *ClusterDatabaseServerValidator) ValidateCreate(_ context.Context, obj *dbv1alpha1.ClusterDatabaseServer) (admission.Warnings, error) {
	serverlog.Info("validating create", "kind", dbv1alpha1.KindClusterDatabaseServer, "name", obj.GetName())
	return nil, validateServer(obj.Spec.Host, obj.Spec.ProxyURL)
}

// ValidateUpdate implements admission.Validator. An object that is being
// deleted is always admitted: the only updates left on it are the finalizer
// removal that completes the delete, and that must never be blocked (for
// example while the webhook's own pod is down).
func (v *ClusterDatabaseServerValidator) ValidateUpdate(_ context.Context, _, newObj *dbv1alpha1.ClusterDatabaseServer) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	serverlog.Info("validating update", "kind", dbv1alpha1.KindClusterDatabaseServer, "name", newObj.GetName())
	return nil, validateServer(newObj.Spec.Host, newObj.Spec.ProxyURL)
}

// ValidateDelete implements admission.Validator; deletes are always admitted
// (the verbs marker above does not even send them here).
func (v *ClusterDatabaseServerValidator) ValidateDelete(_ context.Context, _ *dbv1alpha1.ClusterDatabaseServer) (admission.Warnings, error) {
	return nil, nil
}

// +kubebuilder:webhook:path=/validate-db-craftainer-dev-v1alpha1-databaseserver,mutating=false,failurePolicy=fail,sideEffects=None,groups=db.craftainer.dev,resources=databaseservers,verbs=create;update,versions=v1alpha1,name=vdatabaseserver-v1alpha1.kb.io,admissionReviewVersions=v1

// DatabaseServerValidator validates DatabaseServers on create and update.
type DatabaseServerValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *DatabaseServerValidator) ValidateCreate(_ context.Context, obj *dbv1alpha1.DatabaseServer) (admission.Warnings, error) {
	serverlog.Info("validating create", "kind", dbv1alpha1.KindDatabaseServer, "name", obj.GetName())
	return nil, validateServer(obj.Spec.Host, obj.Spec.ProxyURL)
}

// ValidateUpdate implements admission.Validator; see
// ClusterDatabaseServerValidator.ValidateUpdate for the deletion rule.
func (v *DatabaseServerValidator) ValidateUpdate(_ context.Context, _, newObj *dbv1alpha1.DatabaseServer) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	serverlog.Info("validating update", "kind", dbv1alpha1.KindDatabaseServer, "name", newObj.GetName())
	return nil, validateServer(newObj.Spec.Host, newObj.Spec.ProxyURL)
}

// ValidateDelete implements admission.Validator; deletes are always admitted.
func (v *DatabaseServerValidator) ValidateDelete(_ context.Context, _ *dbv1alpha1.DatabaseServer) (admission.Warnings, error) {
	return nil, nil
}

// validateServer checks the fields both server kinds share, and returns every
// violation, not only the first.
func validateServer(host, proxyURL string) error {
	var errs []error
	if !validHost(host) {
		errs = append(errs, fmt.Errorf("%w: %q", ErrInvalidHost, host))
	}
	if proxyURL != "" && !validProxyURL(proxyURL) {
		errs = append(errs, fmt.Errorf("%w: %q", ErrInvalidProxyURL, proxyURL))
	}
	return errors.Join(errs...)
}

// validHost reports whether host is an IP address or a DNS-1123 subdomain
// (case-insensitive, as host names are).
func validHost(host string) bool {
	return net.ParseIP(host) != nil || len(validation.IsDNS1123Subdomain(strings.ToLower(host))) == 0
}

// validProxyURL reports whether raw is an http, https or socks5 URL with a host.
func validProxyURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https", "socks5":
		return u.Host != ""
	default:
		return false
	}
}
