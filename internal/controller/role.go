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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
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
	// A claim that is being deleted and never issued a create has nothing on the
	// server (roles of its name, if there are any, are not its own): it is
	// released without contacting the server, which may be gone.
	if !role.DeletionTimestamp.IsZero() && role.GetExternalID() == "" {
		return lifecycle.Reconcile[dbv1alpha1.DatabaseRoleSpec, pgclient.RoleState](ctx, r.Client, &role, role.Spec,
			nothingMade[dbv1alpha1.DatabaseRoleSpec, pgclient.RoleState]{}, r.options(nil))
	}
	be, err := r.backend(ctx, role.Namespace, role.Spec.ServerRef)
	opts := r.options(err)
	var ext lifecycle.External[dbv1alpha1.DatabaseRoleSpec, pgclient.RoleState]
	if err == nil {
		opts.ServerHost = be.host
		x := newRoleExternal(be, &role)
		x.persist = func(ctx context.Context) error { return r.Client.Status().Update(ctx, &role) }
		// The database is the one the Database claim names in its status, not
		// one derived again here. Deleting a role does not need the Database
		// (it may be gone first), so a failure only counts when not deleting.
		name, derr := r.checkDatabase(ctx, &role)
		if name != "" {
			x.database = name
		}
		ext = x
		iam := role.Spec.AuthMode == dbv1alpha1.AuthModeIAM
		if iam {
			ext = iamRoleExternal{x}
		}
		if role.DeletionTimestamp.IsZero() {
			x.pre = derr
			if iam {
				x.pre = r.checkIAM(&role, x, derr)
			}
		}
	}
	return lifecycle.Reconcile(ctx, r.Client, &role, role.Spec, ext, opts)
}

// checkIAM is the precondition of an IAM-mode claim, in this order: the server
// must be one whose provider supports IAM (AWS or GCP; AuthModeUnsupported
// otherwise, whatever else is wrong), the rotate annotation is answered with
// RotationNotApplicable, iam.principal must fit the provider (refused on AWS,
// required and valid on GCP: IAMPrincipalRequired), the Database must be usable
// (derr) and, on AWS, the ARN of the login must be known. It records the ARN on
// x.
func (r *RoleReconciler) checkIAM(role *dbv1alpha1.DatabaseRole, x *roleExternal, derr error) error {
	if p := x.be.provider; p != cloud.ProviderAWS && p != cloud.ProviderGCP {
		r.warn(role, ReasonAuthModeUnsupported, ErrAuthModeUnsupported.Error())
		return ErrAuthModeUnsupported
	}
	if role.GetAnnotations()[lifecycle.AnnotationRotate] != "" {
		r.warn(role, ReasonRotationNotApplicable, "an IAM login has no credential to rotate: the consumer's identity authenticates with a short-lived token")
	}
	if err := checkPrincipal(x.be.provider, role.Spec.IAM); err != nil {
		r.warn(role, ReasonIAMPrincipalRequired, err.Error())
		return err
	}
	if derr != nil {
		return derr
	}
	if x.be.provider == cloud.ProviderGCP {
		return nil
	}
	var err error
	x.arn, err = IAMResourceARN(x.be.region, x.be.discovered, x.base)
	return err
}

// ErrIAMPrincipalRefused means iam.principal is set on AWS, where the login is
// named by the operator and the consumer's role is authorized by an ARN.
var ErrIAMPrincipalRefused = errors.New("IAMPrincipalRequired: iam.principal is not used on AWS")

// checkPrincipal checks iam.principal against the provider: refused on AWS,
// required and a valid Cloud SQL user name on GCP.
func checkPrincipal(p cloud.ProviderType, iam *dbv1alpha1.IAMSpec) error {
	switch {
	case p == cloud.ProviderAWS && iam != nil:
		return ErrIAMPrincipalRefused
	case p == cloud.ProviderGCP && (iam == nil || !ValidPrincipal(iam.Principal)):
		return ErrIAMPrincipalInvalid
	}
	return nil
}

// warn records a Warning Event on a claim, when there is a recorder.
func (r *RoleReconciler) warn(role *dbv1alpha1.DatabaseRole, reason, text string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(role, nil, corev1.EventTypeWarning, reason, "Reconcile", "%s", text)
	}
}

// options are the lifecycle options of the controller; resolveErr is the error
// of resolving the server, or nil.
func (r *RoleReconciler) options(resolveErr error) lifecycle.Options {
	return lifecycle.Options{
		Class: lifecycle.ClassCredential, BindingType: binding.TypePostgreSQL, Provider: binding.DefaultProvider,
		ManagedBy: r.ManagedBy, Recorder: r.Recorder, APIReader: r.APIReader, ResolveErr: resolveErr, Now: r.Now,
	}
}

// checkDatabase makes sure the Database exists in the claim's namespace on the
// same server: a role is for one database of the server it names. It returns
// the database's status.externalName, "" while the Database has none yet.
func (r *RoleReconciler) checkDatabase(ctx context.Context, role *dbv1alpha1.DatabaseRole) (string, error) {
	var db dbv1alpha1.Database
	key := client.ObjectKey{Namespace: role.Namespace, Name: role.Spec.DatabaseRef.Name}
	if err := r.Client.Get(ctx, key, &db); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: Database %q not found", ErrDatabaseNotUsable, key.Name)
		}
		return "", err
	}
	if db.Spec.ServerRef != role.Spec.ServerRef {
		return "", fmt.Errorf("%w: Database %q is on %s %q, not on %s %q", ErrDatabaseNotUsable, key.Name,
			db.Spec.ServerRef.Kind, db.Spec.ServerRef.Name, role.Spec.ServerRef.Kind, role.Spec.ServerRef.Name)
	}
	return db.GetExternalName(), nil
}

// databaseRefIndex indexes DatabaseRoles by the name of the Database they use.
const databaseRefIndex = ".spec.databaseRef.name"

func indexDatabaseRef(o client.Object) []string {
	return []string{o.(*dbv1alpha1.DatabaseRole).Spec.DatabaseRef.Name}
}

// rolesOfDatabase maps a Database to the DatabaseRoles of its namespace that
// name it, so a role that waits for its Database is reconciled when the
// Database changes (is created, gets its name, is deleted).
func rolesOfDatabase(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list dbv1alpha1.DatabaseRoleList
		if err := c.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{databaseRefIndex: o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "cannot list the roles of a Database", "database", o.GetName())
			return nil
		}
		reqs := make([]reconcile.Request, len(list.Items))
		for i := range list.Items {
			reqs[i] = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])}
		}
		return reqs
	}
}

// SetupWithManager registers the controller. It also watches the binding
// Secrets it owns (every change, so an edited Secret is repaired) and the
// Databases its claims name (so a role that waits for its Database goes on
// when the Database has its name). Only the claims themselves pass the
// lifecycle filter.
func (r *RoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dbv1alpha1.DatabaseRole{}, databaseRefIndex, indexDatabaseRef); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.DatabaseRole{}, builder.WithPredicates(lifecycle.Predicates())).
		Owns(&corev1.Secret{}).
		Watches(&dbv1alpha1.Database{}, handler.EnqueueRequestsFromMapFunc(rolesOfDatabase(mgr.GetClient()))).
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
	// arn is the iamResourceArn of an IAM-mode claim's login.
	arn string
	// pre is a precondition failure (the Database is not usable). Observe
	// reports it, and Reconcile calls nothing else when Observe fails.
	pre error
	// persist writes the claim's status now, instead of at the end of the
	// reconcile.
	persist func(context.Context) error
}

func newRoleExternal(be *backend, claim *dbv1alpha1.DatabaseRole) *roleExternal {
	base := claim.GetExternalName()
	if base == "" {
		base = pgclient.RoleBase(claim.Namespace, claim.Name)
		// On Cloud SQL the IAM user's name is the principal's, chosen by the claim
		// (ADR 0002 section 16.4). A principal that is not valid is refused by
		// checkIAM, and the derived name is only a placeholder then.
		if be.provider == cloud.ProviderGCP && claim.Spec.AuthMode == dbv1alpha1.AuthModeIAM &&
			claim.Spec.IAM != nil && ValidPrincipal(claim.Spec.IAM.Principal) {
			base = claim.Spec.IAM.Principal
		}
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

// namedRole is a role of a claim's set, with the name it has on the server.
type namedRole struct {
	name string
	info pgclient.RoleInfo
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

// foreign says what to do with a refusal of a role that is not this claim's: a
// claim that is being deleted leaves the roles alone and goes on (nil), any other
// keeps the refusal.
func (e *roleExternal) foreign(refusal error) error {
	if e.claim.DeletionTimestamp.IsZero() {
		return refusal
	}
	return nil
}

// Observe implements lifecycle.External. The role is up to date when its owner
// role cannot log in, both logins exist with the wanted connection limit, the
// active one can log in, and the marker names this claim and its privileges.
//
// While the claim is being deleted, a role that is not its own (any of the
// three) is not refused but left out: Delete drops the roles that are its own
// and skips the others (DropOwnRoles), and a set with none of its own is
// released without a drop. A login of ours is dropped even when the owner role
// next to it is foreign, because it would be a live credential left behind.
func (e *roleExternal) Observe(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec) (lifecycle.Observation[pgclient.RoleState], error) {
	return e.observe(ctx, false)
}

// observe is Observe of both modes. An IAM-mode claim has the one login role
// named base, and neither the _a nor the _b login: it is up to date when that
// role can log in, is (on RDS) a member of rds_iam, has the wanted connection
// limit and carries this claim's marker with its privileges.
func (e *roleExternal) observe(ctx context.Context, iam bool) (lifecycle.Observation[pgclient.RoleState], error) {
	none := lifecycle.Observation[pgclient.RoleState]{}
	// Reconcile adds the finalizer with a patch that replaces the claim with
	// the server's copy, which loses a name set before it; set it again here.
	e.claim.SetExternalName(e.base)
	if e.pre != nil {
		return none, e.pre
	}
	st, err := e.be.admin.Role(ctx, e.base)
	if err != nil {
		return none, err
	}
	// A role is created together with its marker, so one that exists without
	// ours is not ours, whichever of the three it is.
	ours := false
	roles := []namedRole{{e.base, st.Base}}
	if !iam {
		roles = append(roles, namedRole{pgclient.LoginName(e.base, pgclient.GenA), st.A}, namedRole{pgclient.LoginName(e.base, pgclient.GenB), st.B})
	}
	for _, r := range roles {
		if !r.info.Exists {
			continue
		}
		if r.info.Marker.Claim != e.id {
			if err := e.foreign(fmt.Errorf("%w: role %q belongs to another claim or is not managed by this operator",
				lifecycle.ErrNameCollision, r.name)); err != nil {
				return none, err
			}
			continue
		}
		ours = true
	}
	if !e.claim.DeletionTimestamp.IsZero() {
		// Only what is ours is dropped (Delete); none of it means nothing to do.
		return lifecycle.Observation[pgclient.RoleState]{Exists: ours, Current: st}, nil
	}
	if !st.Base.Exists {
		return none, nil
	}
	// A claim from before the create was recorded: the marker is the proof that
	// the roles are ours, so the record is made now (written with the status).
	if st.Marker.Claim == e.id && e.claim.GetExternalID() == "" {
		e.claim.SetExternalID(createIssued)
	}
	upToDate := st.Marker.Claim == e.id && st.Marker.Privileges == e.privileges
	if iam {
		upToDate = upToDate && st.Base.Login && st.Base.ConnLimit == e.limit &&
			(e.be.provider != cloud.ProviderAWS || st.Base.RDSIAM)
	} else {
		upToDate = upToDate && !st.Base.Login &&
			st.A.Exists && st.B.Exists && st.A.ConnLimit == e.limit && st.B.ConnLimit == e.limit &&
			st.Login(activeOf(st.Marker)).Login
	}
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
//
// The create is recorded in status.externalId before the first statement, so a
// crash after it is recognised; a claim deleted without the record is released
// without the server being contacted.
func (e *roleExternal) Create(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec) (binding.Data, error) {
	return e.create(ctx, func(ctx context.Context) (binding.Data, error) { return e.issue(ctx, pgclient.GenA) })
}

// create records the create and runs issue, which creates the roles of the
// claim's mode.
func (e *roleExternal) create(ctx context.Context, issue func(context.Context) (binding.Data, error)) (binding.Data, error) {
	e.claim.SetExternalID(createIssued)
	if err := e.persist(ctx); err != nil {
		return nil, err
	}
	data, err := issue(ctx)
	if !errors.Is(err, pgclient.ErrNotOwned) {
		return data, err
	}
	// Somebody made a role of the name between Observe and the create. Nothing
	// of this claim is on the server (ApplyRole creates nothing for a role it
	// refuses), so the record is withdrawn and the claim is released on deletion
	// without contacting it.
	e.claim.SetExternalID("")
	return nil, errors.Join(err, e.persist(ctx))
}

// Update implements lifecycle.External. It re-issues the active login's
// password, because the Secret has to be written again with a value.
func (e *roleExternal) Update(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec, cur pgclient.RoleState) (binding.Data, error) {
	return e.issue(ctx, activeOf(cur.Marker))
}

// Delete implements lifecycle.External: every generation that is the claim's
// goes with it; a role of the set that is not is skipped.
func (e *roleExternal) Delete(ctx context.Context, _ pgclient.RoleState) error {
	skipped, err := e.be.admin.DropOwnRoles(ctx, e.base, e.id, e.database)
	if len(skipped) > 0 {
		log.FromContext(ctx).Info("roles that are not this claim's were left alone", "roles", skipped)
	}
	return err
}

// RotateBegin implements lifecycle.Rotator: the new generation is the login
// that is not active (a is the first one, so the first rotation issues b). It
// is the same login on a retry, whatever crashed before.
func (e *roleExternal) RotateBegin(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec, _ pgclient.RoleState, st lifecycle.RotationState) (binding.Data, lifecycle.Generation, error) {
	next := pgclient.OtherGen(string(st.Active))
	// The same call as Create and Update, with the new login active: it also
	// applies the privileges, so the marker it writes names privileges that
	// were applied.
	data, err := e.issue(ctx, next)
	if err != nil {
		return nil, "", err
	}
	return data, lifecycle.Generation(next), nil
}

// RotateFinish implements lifecycle.Rotator. st.Previous is empty during the
// first grace period, so the login to retire is the one that is not active.
//
// Ending the sessions of the retired login is best effort: an admin role that
// lacks pg_signal_backend cannot, and the retire still succeeds, because NOLOGIN
// and the cleared password already refuse every new session. It is reported
// with a log line, a Warning Event and the SessionsTerminated condition (False,
// SessionsNotTerminated, a fixed text); the condition goes away when a later
// retire ends the sessions.
func (e *roleExternal) RotateFinish(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec, _ pgclient.RoleState, st lifecycle.RotationState) error {
	err := e.be.admin.Retire(ctx, e.base, pgclient.OtherGen(string(st.Active)))
	if !errors.Is(err, pgclient.ErrSessionsNotEnded) {
		if err == nil {
			meta.RemoveStatusCondition(e.claim.GetConditions(), ConditionSessionsTerminated)
		}
		return err
	}
	log.FromContext(ctx).Info("the retired login was disabled, but its open sessions were not ended: "+
		"the admin role needs pg_signal_backend", "login", pgclient.LoginName(e.base, pgclient.OtherGen(string(st.Active))))
	if e.be.recorder != nil {
		e.be.recorder.Eventf(e.claim, nil, corev1.EventTypeWarning, ReasonSessionsNotTerminated, "Rotate", "%s", sessionsNotTerminatedText)
	}
	meta.SetStatusCondition(e.claim.GetConditions(), metav1.Condition{
		Type: ConditionSessionsTerminated, Status: metav1.ConditionFalse, Reason: ReasonSessionsNotTerminated,
		Message: sessionsNotTerminatedText, ObservedGeneration: e.claim.GetGeneration(),
	})
	return nil
}

// ConditionSessionsTerminated and ReasonSessionsNotTerminated report that a
// retired login's open sessions could not be ended (see RotateFinish).
const (
	ConditionSessionsTerminated = "SessionsTerminated"
	ReasonSessionsNotTerminated = "SessionsNotTerminated"
	sessionsNotTerminatedText   = "the previous login is disabled, but its open sessions were not ended: " +
		"the admin role needs pg_signal_backend"
)

// endpoint is where and how a login connects: the host, the port and the
// sslmode of the binding.
func (e *roleExternal) endpoint() (host string, port int32, sslmode string) {
	host, port = e.be.host, e.be.port
	if port == 0 {
		port = dbv1alpha1.DefaultPort
	}
	sslmode = string(e.be.spec.SSLMode)
	if sslmode == "" {
		sslmode = string(dbv1alpha1.SSLModeVerifyFull)
	}
	return host, port, sslmode
}

// data is the postgresql binding of one login (ADR 0002, section 7). The CA is
// the server's bundle; the uri carries the password, like the Secret does.
func (e *roleExternal) data(gen, password string) binding.Data {
	user := pgclient.LoginName(e.base, gen)
	host, port, sslmode := e.endpoint()
	hostPort := net.JoinHostPort(host, strconv.Itoa(int(port)))
	uri := url.URL{
		Scheme: "postgresql", User: url.UserPassword(user, password), Host: hostPort,
		Path: "/" + e.database, RawQuery: url.Values{"sslmode": {sslmode}}.Encode(),
	}
	d := binding.Data{
		"host":        []byte(host),
		"port":        []byte(strconv.Itoa(int(port))),
		"database":    []byte(e.database),
		"username":    []byte(user),
		"password":    []byte(password),
		"sslmode":     []byte(sslmode),
		"sslrootcert": e.be.conn.CABundle,
		"uri":         []byte(uri.String()),
	}
	if e.be.cloud {
		// ADR 0002 section 16.5: the keys every cloud binding adds.
		d["authMode"] = []byte(dbv1alpha1.AuthModePassword)
		e.cloudKeys(d)
	}
	return d
}

// cloudKeys adds the keys of a cloud binding that depend on the provider: the
// cloud, and the AWS region or the Cloud SQL connection name.
func (e *roleExternal) cloudKeys(d binding.Data) {
	if e.be.provider == cloud.ProviderGCP {
		d["cloud"] = []byte("gcp")
		d["instanceConnectionName"] = []byte(e.be.cs.connectionName())
		return
	}
	d["cloud"] = []byte("aws")
	d["region"] = []byte(e.be.region)
}

// iamRoleExternal is the lifecycle.External of an IAM-mode DatabaseRole (ADR
// 0005): one login role, authenticated by an IAM token the consumer builds.
// It is deliberately not a lifecycle.Rotator, and does not embed the
// roleExternal that is one: nothing is rotated, and status.rotation stays
// empty. The rotate annotation is answered by the controller.
type iamRoleExternal struct{ x *roleExternal }

// Observe implements lifecycle.External.
func (e iamRoleExternal) Observe(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec) (lifecycle.Observation[pgclient.RoleState], error) {
	return e.x.observe(ctx, true)
}

// Create implements lifecycle.External, with the same record of the create
// as a Password-mode claim.
func (e iamRoleExternal) Create(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec) (binding.Data, error) {
	return e.x.create(ctx, e.x.issueIAM)
}

// Update implements lifecycle.External. It applies the role again (privileges
// and connection limit) and returns the same binding data.
func (e iamRoleExternal) Update(ctx context.Context, _ dbv1alpha1.DatabaseRoleSpec, _ pgclient.RoleState) (binding.Data, error) {
	return e.x.issueIAM(ctx)
}

// Delete implements lifecycle.External: the role is dropped as in Password
// mode, when it is the claim's; on Cloud SQL its IAM user is deleted too.
func (e iamRoleExternal) Delete(ctx context.Context, cur pgclient.RoleState) error {
	if e.x.be.provider == cloud.ProviderGCP {
		return e.x.deleteCloudSQLUser(ctx)
	}
	return e.x.Delete(ctx, cur)
}

// issueIAM applies the IAM-mode login role and returns the binding data.
func (e *roleExternal) issueIAM(ctx context.Context) (binding.Data, error) {
	p := pgclient.RoleParams{
		Base: e.base, Database: e.database, Marker: e.marker(""), ConnLimit: e.limit, Privileges: e.privileges,
	}
	var err error
	if e.be.provider == cloud.ProviderGCP {
		err = e.be.admin.ApplyCloudSQLIAMRole(ctx, p, e.insertCloudSQLUser)
	} else {
		err = e.be.admin.ApplyIAMRole(ctx, p)
	}
	if err != nil {
		return nil, err
	}
	return e.iamData(), nil
}

// insertCloudSQLUser creates the IAM database user of a Cloud SQL claim, named
// by the principal, with the Admin API.
func (e *roleExternal) insertCloudSQLUser(ctx context.Context) error {
	api, done, err := e.be.sqlAdmin(ctx)
	if err != nil {
		return err
	}
	defer done()
	return insertIAMUser(ctx, api, e.be.cs, e.base)
}

// deleteCloudSQLUser removes what is the claim's of a Cloud SQL IAM user: the
// objects it owns go to the admin role and the Admin API deletes the user. A
// role that is not the claim's is skipped.
func (e *roleExternal) deleteCloudSQLUser(ctx context.Context) error {
	skipped, err := e.be.admin.DropCloudSQLIAMRole(ctx, e.base, e.id, e.database, func(ctx context.Context) error {
		api, done, err := e.be.sqlAdmin(ctx)
		if err != nil {
			return err
		}
		defer done()
		return deleteIAMUser(ctx, api, e.be.cs, e.base)
	})
	if skipped {
		log.FromContext(ctx).Info("the role is not this claim's and was left alone", "roles", []string{e.base})
	}
	return err
}

// iamData is the binding of an IAM-mode login (docs/crd-drafts.md, "db x
// cloud"): no password and no uri, which would carry one. On AWS the consumer
// builds a token for username with the AWS SDK and needs the region and the ARN
// its role may connect to; on Cloud SQL username is the principal and the
// consumer connects through the Cloud SQL connector with IAM authentication,
// which needs the instance connection name.
func (e *roleExternal) iamData() binding.Data {
	host, port, sslmode := e.endpoint()
	d := binding.Data{
		"host":        []byte(host),
		"port":        []byte(strconv.Itoa(int(port))),
		"database":    []byte(e.database),
		"username":    []byte(e.base),
		"sslmode":     []byte(sslmode),
		"sslrootcert": e.be.conn.CABundle,
		"authMode":    []byte(dbv1alpha1.AuthModeIAM),
	}
	e.cloudKeys(d)
	if e.be.provider == cloud.ProviderAWS {
		d["iamResourceArn"] = []byte(e.arn)
	}
	return d
}

var _ lifecycle.Rotator[dbv1alpha1.DatabaseRoleSpec, pgclient.RoleState] = (*roleExternal)(nil)
