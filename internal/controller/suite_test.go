package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/testenv"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/egress"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// The reconcile flow runs against envtest's kube-apiserver with a fake
// PostgreSQL behind pgclient.Session, so it needs no database. The real
// PostgreSQL integration lane is NO2b-2. The suite needs the binaries
// scripts/go-coverage.sh and `make test` fetch with setup-envtest.

var env *testenv.Env

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(dbv1alpha1.AddToScheme(scheme))
	var err error
	env, err = testenv.Start(context.Background(), testenv.Options{
		Scheme: scheme, CRDPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start envtest:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot stop envtest:", err)
		code = 1
	}
	os.Exit(code)
}

// pgFake is a PostgreSQL that records what it is asked and answers queries
// with answer.
type pgFake struct {
	mu      sync.Mutex
	stmts   []string
	targets []pgclient.Target
	answer  func(sql string) []pgclient.Row
	connErr error
	// failOn makes the statements that contain it fail.
	failOn string
}

var (
	errUnreachable = errors.New("connection refused")
	errStatement   = errors.New("statement failed")
)

func (f *pgFake) connect(_ context.Context, t pgclient.Target) (pgclient.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, t)
	if f.connErr != nil {
		return nil, f.connErr
	}
	return &pgSession{f: f, db: t.Database}, nil
}

func (f *pgFake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stmts = nil
}

func (f *pgFake) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.stmts, "\n")
}

func (f *pgFake) want(t *testing.T, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(f.joined(), s) {
			t.Errorf("no statement contains %q; statements:\n%s", s, f.joined())
		}
	}
}

func (f *pgFake) wantNot(t *testing.T, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(f.joined(), s) {
			t.Errorf("a statement contains %q; statements:\n%s", s, f.joined())
		}
	}
}

type pgSession struct {
	f  *pgFake
	db string
}

func (s *pgSession) Exec(_ context.Context, sql string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.stmts = append(s.f.stmts, s.db+"|"+sql)
	if s.f.failOn != "" && strings.Contains(sql, s.f.failOn) {
		return errStatement
	}
	return nil
}

func (s *pgSession) Query(_ context.Context, sql string) ([]pgclient.Row, error) {
	s.f.mu.Lock()
	s.f.stmts = append(s.f.stmts, s.db+"|"+sql)
	answer := s.f.answer
	s.f.mu.Unlock()
	if answer == nil {
		return nil, nil
	}
	return answer(sql), nil
}

func (s *pgSession) Close(context.Context) {}

// harness is one test's namespaces, server, recorder and fake PostgreSQL.
type harness struct {
	t      *testing.T
	ns     string // the tenant namespace
	ops    string // the operator namespace, holding the admin Secret
	server *dbv1alpha1.ClusterDatabaseServer
	fake   *pgFake
	rec    *testenv.Recorder
	cfg    Config
	now    time.Time
}

func newHarness(t *testing.T, policy common.DeletionPolicy, mutate func(*dbv1alpha1.ClusterDatabaseServer)) *harness {
	t.Helper()
	h := &harness{t: t, ns: env.Namespace(t, nil), ops: env.Namespace(t, nil), fake: &pgFake{}, rec: &testenv.Recorder{}, now: time.Now()}
	name := "pg-" + strings.ToLower(rand.Text())[:10]
	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-admin", Namespace: h.ops},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("admin-pw")},
	}
	srv := &dbv1alpha1.ClusterDatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: name}}
	srv.Spec.Host = "pg.example.test"
	srv.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: admin.Name, Namespace: h.ops}
	srv.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	srv.Spec.AllowedExtensions = []string{"pgcrypto"}
	if mutate != nil {
		mutate(srv)
	}
	h.must(env.Client.Create(context.Background(), admin))
	h.must(env.Client.Create(context.Background(), srv))
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), srv) })
	h.server = srv
	h.cfg = Config{
		Client: env.Client, APIReader: env.Client, Recorder: h.rec, ManagedBy: "operator-db-test",
		OperatorNamespace: h.ops, DefaultDeletionPolicy: policy, Egress: egress.NewPolicy(egress.ModeStrict),
		Connect: h.fake.connect, Now: func() time.Time { return h.now },
	}
	return h
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) ref() common.ServerRef {
	return common.ServerRef{Kind: dbv1alpha1.KindClusterDatabaseServer, Name: h.server.Name}
}

func (h *harness) database(name string, mutate func(*dbv1alpha1.Database)) *dbv1alpha1.Database {
	h.t.Helper()
	db := &dbv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns}}
	db.Spec.ServerRef = h.ref()
	if mutate != nil {
		mutate(db)
	}
	h.must(env.Client.Create(context.Background(), db))
	return db
}

func (h *harness) role(name, database string, mutate func(*dbv1alpha1.DatabaseRole)) *dbv1alpha1.DatabaseRole {
	h.t.Helper()
	r := &dbv1alpha1.DatabaseRole{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns}}
	r.Spec.ServerRef = h.ref()
	r.Spec.DatabaseRef = dbv1alpha1.DatabaseRef{Name: database}
	if mutate != nil {
		mutate(r)
	}
	h.must(env.Client.Create(context.Background(), r))
	return r
}

type reconciler interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}

func (h *harness) reconcile(r reconciler, o client.Object) error {
	h.t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(o)})
	return err
}

func (h *harness) reconcileOK(r reconciler, o client.Object) {
	h.t.Helper()
	h.must(h.reconcile(r, o))
}

func (h *harness) get(o client.Object) {
	h.t.Helper()
	h.must(env.Client.Get(context.Background(), client.ObjectKeyFromObject(o), o))
}

func (h *harness) gone(o client.Object) bool {
	h.t.Helper()
	err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(o), o)
	return apierrors.IsNotFound(err)
}

// condition returns the status and reason of a condition of a claim.
func condition(conds []metav1.Condition, typ string) (metav1.ConditionStatus, string, string) {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return "", "", ""
	}
	return c.Status, c.Reason, c.Message
}

func (h *harness) wantCondition(conds []metav1.Condition, typ string, status metav1.ConditionStatus, reason string) string {
	h.t.Helper()
	s, r, msg := condition(conds, typ)
	if s != status || r != reason {
		h.t.Errorf("condition %s = %s/%s (%s), want %s/%s", typ, s, r, msg, status, reason)
	}
	return msg
}
