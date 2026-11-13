package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// ErrDatabaseNotUsable means the Database a DatabaseRole names is missing or
// lives on another server.
var ErrDatabaseNotUsable = errors.New("the referenced Database cannot be used")

// RoleReconciler reconciles DatabaseRole claims (a credential kind).
type RoleReconciler struct{ Config }

// Reconcile implements reconcile.Reconciler.
func (r *RoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var role dbv1alpha1.DatabaseRole
	if err := r.Client.Get(ctx, req.NamespacedName, &role); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	be, err := r.backend(ctx, role.Namespace, role.Spec.ServerRef)
	opts := lifecycle.Options{
		Class: lifecycle.ClassCredential, BindingType: binding.TypePostgreSQL, Provider: binding.DefaultProvider,
		ManagedBy: r.ManagedBy, Recorder: r.Recorder, APIReader: r.APIReader, ResolveErr: err,
		Now: r.Now,
	}
	var ext lifecycle.External[dbv1alpha1.DatabaseRoleSpec, pgclient.RoleState]
	if err == nil {
		opts.ServerHost = be.spec.Host
		x := newRoleExternal(be, &role)
		if role.DeletionTimestamp.IsZero() {
			x.pre = r.checkDatabase(ctx, &role)
		}
		ext = x
	}
	return lifecycle.Reconcile(ctx, r.Client, &role, role.Spec, ext, opts)
}

// checkDatabase makes sure the Database exists in the claim's namespace on the
// same server: a role is for one database of the server it names.
func (r *RoleReconciler) checkDatabase(ctx context.Context, role *dbv1alpha1.DatabaseRole) error {
	var db dbv1alpha1.Database
	key := client.ObjectKey{Namespace: role.Namespace, Name: role.Spec.DatabaseRef.Name}
	if err := r.Client.Get(ctx, key, &db); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: Database %q not found", ErrDatabaseNotUsable, key.Name)
		}
		return err
	}
	if db.Spec.ServerRef != role.Spec.ServerRef {
		return fmt.Errorf("%w: Database %q is on %s %q, not on %s %q", ErrDatabaseNotUsable, key.Name,
			db.Spec.ServerRef.Kind, db.Spec.ServerRef.Name, role.Spec.ServerRef.Kind, role.Spec.ServerRef.Name)
	}
	return nil
}

// SetupWithManager registers the controller; it also watches the binding
// Secrets it owns.
func (r *RoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.DatabaseRole{}).
		Owns(&corev1.Secret{}).
		WithEventFilter(lifecycle.Predicates()).
		Named("databaserole").
		Complete(r)
}

// roleExternal is the lifecycle.Rotator of one DatabaseRole (ADR 0002,
// section 8): a NOLOGIN owner role and two logins, a and b.
type roleExternal struct {
	be         *backend
	claim      *dbv1alpha1.DatabaseRole
	base       string
	database   string
	id         string
	privileges string
	limit      int32
	// pre is a precondition failure (the Database is not usable). Observe
	// reports it, and Reconcile calls nothing else when Observe fails.
	pre error
}

func newRoleExternal(be *backend, claim *dbv1alpha1.DatabaseRole) *roleExternal {
	base := claim.GetExternalName()
	if base == "" {
		base = pgclient.RoleBase(claim.Namespace, claim.Name)
		claim.SetExternalName(base)
	}
	priv := string(claim.Spec.Privileges)
	if priv == "" {
		priv = pgclient.ReadWrite
	}
	return &roleExternal{
		be: be, claim: claim, base: base, database: pgclient.DatabaseName(claim.Namespace, claim.Spec.DatabaseRef.Name),
		id: claimID(claim), privileges: priv, limit: connLimit(claim.Spec.ConnectionLimit),
	}
}

func (e *roleExternal) marker(active string) pgclient.Marker {
	return pgclient.Marker{Claim: e.id, Privileges: e.privileges, Active: active}
}

func activeOf(m pgclient.Marker) string {
	if m.Active == "" {
		return pgclient.GenA
	}
	return m.Active
}

// Observe implements lifecycle.External. The role is up to date when its owner
// role cannot log in, both logins exist with the wanted connection limit, the
// active one can log in, and the marker names this claim and its privileges.
func (e *roleExternal) Observe(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec) (lifecycle.Observation[pgclient.RoleState], error) {
	none := lifecycle.Observation[pgclient.RoleState]{}
	// Reconcile adds the finalizer with a patch that replaces the claim with
	// the server's copy, which loses a name set before it; set it again here.
	e.claim.SetExternalName(e.base)
	if e.pre != nil {
		return none, e.pre
	}
	st, err := e.be.admin.Role(ctx, e.base)
	if err != nil || !st.Base.Exists {
		return none, err
	}
	if st.Marker.Claim != "" && st.Marker.Claim != e.id {
		return none, fmt.Errorf("%w: role %q belongs to another claim", lifecycle.ErrNameCollision, e.base)
	}
	upToDate := st.Marker.Claim == e.id && st.Marker.Privileges == e.privileges && !st.Base.Login &&
		st.A.Exists && st.B.Exists && st.A.ConnLimit == e.limit && st.B.ConnLimit == e.limit &&
		st.Login(activeOf(st.Marker)).Login
	return lifecycle.Observation[pgclient.RoleState]{Exists: true, UpToDate: upToDate, Current: st}, nil
}

// issue applies the role with a fresh password for the active login and
// returns the binding data. A fresh password is the only way to hand the
// binding Secret a value PostgreSQL cannot be asked for.
func (e *roleExternal) issue(ctx context.Context, active string) (binding.Data, error) {
	pw := rand.Text() + rand.Text()
	err := e.be.admin.ApplyRole(ctx, pgclient.RoleParams{
		Base: e.base, Database: e.database, Marker: e.marker(active), ConnLimit: e.limit,
		Password: pw, Privileges: e.privileges,
	})
	if err != nil {
		return nil, err
	}
	return e.data(active, pw), nil
}

// Create implements lifecycle.External: login a is the first generation.
func (e *roleExternal) Create(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec) (binding.Data, error) {
	return e.issue(ctx, pgclient.GenA)
}

// Update implements lifecycle.External. It re-issues the active login's
// password, because the Secret has to be written again with a value.
func (e *roleExternal) Update(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec, cur pgclient.RoleState) (binding.Data, error) {
	return e.issue(ctx, activeOf(cur.Marker))
}

// Delete implements lifecycle.External: every generation goes with the claim.
func (e *roleExternal) Delete(ctx context.Context, _ pgclient.RoleState) error {
	return e.be.admin.DropRole(ctx, e.base, e.database)
}

// RotateBegin implements lifecycle.Rotator: the new generation is the login
// that is not active (a is the first one, so the first rotation issues b). It
// is the same login on a retry, whatever crashed before.
func (e *roleExternal) RotateBegin(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec, _ pgclient.RoleState, st lifecycle.RotationState) (binding.Data, lifecycle.Generation, error) {
	next := pgclient.OtherGen(string(st.Active))
	pw := rand.Text() + rand.Text()
	if err := e.be.admin.Activate(ctx, e.base, next, pw, e.marker(next)); err != nil {
		return nil, "", err
	}
	return e.data(next, pw), lifecycle.Generation(next), nil
}

// RotateFinish implements lifecycle.Rotator. st.Previous is empty during the
// first grace period, so the login to retire is the one that is not active.
func (e *roleExternal) RotateFinish(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec, _ pgclient.RoleState, st lifecycle.RotationState) error {
	return e.be.admin.Retire(ctx, e.base, pgclient.OtherGen(string(st.Active)))
}

// data is the postgresql binding of one login (ADR 0002, section 7). The CA is
// the server's bundle; the uri carries the password, like the Secret does.
func (e *roleExternal) data(gen, password string) binding.Data {
	user := pgclient.LoginName(e.base, gen)
	port := e.be.spec.Port
	if port == 0 {
		port = dbv1alpha1.DefaultPort
	}
	hostPort := net.JoinHostPort(e.be.spec.Host, strconv.Itoa(int(port)))
	sslmode := string(e.be.spec.SSLMode)
	if sslmode == "" {
		sslmode = string(dbv1alpha1.SSLModeVerifyFull)
	}
	uri := url.URL{
		Scheme: "postgresql", User: url.UserPassword(user, password), Host: hostPort,
		Path: "/" + e.database, RawQuery: url.Values{"sslmode": {sslmode}}.Encode(),
	}
	return binding.Data{
		"host":        []byte(e.be.spec.Host),
		"port":        []byte(strconv.Itoa(int(port))),
		"database":    []byte(e.database),
		"username":    []byte(user),
		"password":    []byte(password),
		"sslmode":     []byte(sslmode),
		"sslrootcert": e.be.conn.CABundle,
		"uri":         []byte(uri.String()),
	}
}

var _ lifecycle.Rotator[dbv1alpha1.DatabaseRoleSpec, pgclient.RoleState] = (*roleExternal)(nil)
