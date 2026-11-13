// Package controller reconciles Database and DatabaseRole claims with
// lib-operator-go's lifecycle.Reconcile (ADR 0002): it resolves the claim's
// server, opens the PostgreSQL admin connection through the egress policy and
// hands the framework an External that runs the SQL of package pgclient.
package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/egress"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// Keys of the admin Secret (docs/crd-drafts.md).
const (
	AdminUsernameKey = "username"
	AdminPasswordKey = "password"
)

// ErrAdminCredentials means the admin Secret lacks the username or password.
var ErrAdminCredentials = errors.New("the admin Secret needs the keys username and password")

// ConnectFunc opens a PostgreSQL session; production uses pgclient.Connect.
type ConnectFunc func(ctx context.Context, t pgclient.Target) (pgclient.Session, error)

// Config is what both reconcilers share.
type Config struct {
	// Client reads and writes claims, servers and Secrets. Its Secret reads must
	// bypass the managed-by cache, which cannot see the admin Secret.
	Client client.Client
	// APIReader looks for binding Secrets (lifecycle.Options.APIReader).
	APIReader client.Reader
	Recorder  events.EventRecorder
	// ManagedBy is the managed-by label of binding Secrets and the audit
	// controller name.
	ManagedBy             string
	OperatorNamespace     string
	DefaultDeletionPolicy common.DeletionPolicy
	Egress                egress.Policy
	Connect               ConnectFunc
	// Now is the clock of the lifecycle (rotation and grace periods); nil is
	// time.Now. Tests advance it.
	Now func() time.Time
}

// backend is a resolved server.
type backend struct {
	admin pgclient.Admin
	spec  dbv1alpha1.PostgreSQL
	conn  server.Connection
}

// backend resolves ref for a claim in ns: the connection settings through
// server.Resolve (which enforces allowedNamespaces), the server's PostgreSQL
// fields, and the admin Admin whose connections dial through the egress policy
// and the server's proxy.
func (c Config) backend(ctx context.Context, ns string, ref common.ServerRef) (*backend, error) {
	conn, err := server.Resolve(ctx, c.Client, ns, ref, server.WithOperatorNamespace(c.OperatorNamespace))
	if err != nil {
		return nil, err
	}
	spec, err := c.serverSpec(ctx, ns, ref)
	if err != nil {
		return nil, err
	}
	dial, err := c.Egress.Dialer(conn.Proxy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	user, pass := conn.Admin[AdminUsernameKey], conn.Admin[AdminPasswordKey]
	if len(user) == 0 || len(pass) == 0 {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrAdminCredentials)
	}
	target := pgclient.Target{
		Host: spec.Host, Port: portOf(spec.Port), User: string(user), Password: string(pass),
		ServerName: conn.ServerName, CABundle: conn.CABundle, MinTLS: conn.MinTLSVersion, Dial: dial,
	}
	open := func(ctx context.Context, database string) (pgclient.Session, error) {
		t := target
		t.Database = database
		if t.Database == "" {
			t.Database = spec.AdminDatabase
		}
		return c.Connect(ctx, t)
	}
	return &backend{admin: pgclient.Admin{Open: open}, spec: spec, conn: conn}, nil
}

// portOf is the TCP port of a server. The schema admits 1 to 65535 and
// defaults to 5432, so anything else is a server object the API server did not
// validate, and gets the default.
func portOf(p int32) uint16 {
	if p < 1 || p > math.MaxUint16 {
		return uint16(dbv1alpha1.DefaultPort)
	}
	return uint16(p)
}

// serverSpec reads the PostgreSQL fields of the server, which
// server.Connection does not carry (ADR 0002, open issues).
func (c Config) serverSpec(ctx context.Context, ns string, ref common.ServerRef) (dbv1alpha1.PostgreSQL, error) {
	if ref.Kind == dbv1alpha1.KindClusterDatabaseServer {
		var s dbv1alpha1.ClusterDatabaseServer
		err := c.Client.Get(ctx, client.ObjectKey{Name: ref.Name}, &s)
		return s.Spec.PostgreSQL, err
	}
	var s dbv1alpha1.DatabaseServer
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s)
	return s.Spec.PostgreSQL, err
}

// claimID is the claim part of the ownership marker.
func claimID(o client.Object) string {
	return fmt.Sprintf("%s/%s/%s", o.GetNamespace(), o.GetName(), o.GetUID())
}

// connLimit is spec.connectionLimit, -1 (unlimited) when unset.
func connLimit(p *int32) int32 {
	if p == nil {
		return -1
	}
	return *p
}
