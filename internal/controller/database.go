package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// ErrExtensionNotAllowed means a Database asks for an extension its server's
// allowedExtensions does not list.
var ErrExtensionNotAllowed = errors.New("extension is not in the server's allowedExtensions")

// DatabaseReconciler reconciles Database claims (a data kind).
type DatabaseReconciler struct{ Config }

// Reconcile implements reconcile.Reconciler.
func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var db dbv1alpha1.Database
	if err := r.Client.Get(ctx, req.NamespacedName, &db); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	be, err := r.backend(ctx, db.Namespace, db.Spec.ServerRef)
	opts := lifecycle.Options{
		Class: lifecycle.ClassData, ManagedBy: r.ManagedBy, Recorder: r.Recorder, APIReader: r.APIReader,
		DefaultDeletionPolicy: r.DefaultDeletionPolicy, ResolveErr: err,
		Now: r.Now,
	}
	var ext lifecycle.External[dbv1alpha1.DatabaseSpec, pgclient.DatabaseState]
	if err == nil {
		opts.ServerHost = be.spec.Host
		ext = newDatabaseExternal(be, &db)
	}
	return lifecycle.Reconcile(ctx, r.Client, &db, db.Spec, ext, opts)
}

// SetupWithManager registers the controller.
func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.Database{}).
		WithEventFilter(lifecycle.Predicates()).
		Named("database").
		Complete(r)
}

// databaseExternal is the lifecycle.External of one Database.
type databaseExternal struct {
	be     *backend
	name   string
	marker pgclient.Marker
}

func newDatabaseExternal(be *backend, claim *dbv1alpha1.Database) *databaseExternal {
	// Observe goes by status.externalName, never by re-deriving (ADR 0002,
	// section 3); the name is derived once, at the first reconcile.
	name := claim.GetExternalName()
	if name == "" {
		name = pgclient.DatabaseName(claim.Namespace, claim.Name)
		claim.SetExternalName(name)
	}
	return &databaseExternal{be: be, name: name, marker: pgclient.Marker{Claim: claimID(claim)}}
}

// checkExtensions, run before Create and Update (never before Delete, which
// must not be blocked by a spec that has become invalid), refuses an extension that is not an identifier (the CRD
// pattern, checked again here) or that the server does not allow.
func (e *databaseExternal) checkExtensions(spec dbv1alpha1.DatabaseSpec) error {
	if err := pgclient.ValidateExtensions(spec.Extensions); err != nil {
		return err
	}
	for _, x := range spec.Extensions {
		if !slices.Contains(e.be.spec.AllowedExtensions, x) {
			return fmt.Errorf("%w: %q", ErrExtensionNotAllowed, x)
		}
	}
	return nil
}

func (e *databaseExternal) params(spec dbv1alpha1.DatabaseSpec) pgclient.DatabaseParams {
	return pgclient.DatabaseParams{
		Name: e.name, Marker: e.marker, Locale: spec.Locale, ConnLimit: connLimit(spec.ConnectionLimit),
		Extensions: spec.Extensions,
	}
}

// Observe implements lifecycle.External. A database whose marker names another
// claim is refused; one with no marker is a Create that stopped before it
// stamped the marker, and is taken over by Update.
func (e *databaseExternal) Observe(ctx context.Context, spec dbv1alpha1.DatabaseSpec) (lifecycle.Observation[pgclient.DatabaseState], error) {
	st, err := e.be.admin.Database(ctx, e.name)
	if err != nil || !st.Exists {
		return lifecycle.Observation[pgclient.DatabaseState]{}, err
	}
	if st.Marker.Claim != "" && st.Marker.Claim != e.marker.Claim {
		return lifecycle.Observation[pgclient.DatabaseState]{},
			fmt.Errorf("%w: database %q belongs to another claim", lifecycle.ErrNameCollision, e.name)
	}
	missing := slices.ContainsFunc(spec.Extensions, func(x string) bool { return !slices.Contains(st.Extensions, x) })
	upToDate := st.Marker.Claim == e.marker.Claim && st.ConnLimit == connLimit(spec.ConnectionLimit) && !missing
	return lifecycle.Observation[pgclient.DatabaseState]{Exists: true, UpToDate: upToDate, Current: st}, nil
}

// Create implements lifecycle.External; a data kind has no binding data.
func (e *databaseExternal) Create(ctx context.Context, spec dbv1alpha1.DatabaseSpec) (binding.Data, error) {
	if err := e.checkExtensions(spec); err != nil {
		return nil, err
	}
	return nil, e.be.admin.CreateDatabase(ctx, e.params(spec))
}

// Update implements lifecycle.External. It never drops an extension.
func (e *databaseExternal) Update(ctx context.Context, spec dbv1alpha1.DatabaseSpec, _ pgclient.DatabaseState) (binding.Data, error) {
	if err := e.checkExtensions(spec); err != nil {
		return nil, err
	}
	return nil, e.be.admin.UpdateDatabase(ctx, e.params(spec))
}

// Delete implements lifecycle.External.
func (e *databaseExternal) Delete(ctx context.Context, _ pgclient.DatabaseState) error {
	return e.be.admin.DropDatabase(ctx, e.name)
}
