package controller

import (
	"context"
	"fmt"

	"github.com/craftainer-labs/lib-operator-go/pkg/probe"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// Prober is the probe.Prober of both server kinds: it opens an admin session
// through the egress policy and the server's proxy and TLS settings (the same
// target the claims' reconcilers use) and reads server_version. The caller
// (probe.Reconciler) bounds it by Options.Timeout and shows only that it failed;
// the error text names a category of failure, never a credential.
type Prober struct{ Config Config }

var _ probe.Prober = Prober{}

// Probe returns the server_version of srv. The session is opened for the probe
// and closed with it, not taken from the claims' pool cache, so a probe of a
// server nobody claims leaves nothing behind.
func (p Prober) Probe(ctx context.Context, srv server.Server, conn server.Connection) (string, error) {
	spec, err := postgresOf(srv)
	if err != nil {
		return "", err
	}
	dial, err := p.Config.Egress.Dialer(conn.Proxy)
	if err != nil {
		return "", fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	target, err := adminTarget(spec, conn, dial)
	if err != nil {
		return "", err
	}
	admin := pgclient.Admin{Open: pgclient.Opener(nil, p.Config.Connect, target)}
	return admin.ServerVersion(ctx)
}

// postgresOf returns the PostgreSQL fields of a server kind of this operator.
func postgresOf(srv server.Server) (dbv1alpha1.PostgreSQL, error) {
	switch s := srv.(type) {
	case *dbv1alpha1.ClusterDatabaseServer:
		return s.Spec.PostgreSQL, nil
	case *dbv1alpha1.DatabaseServer:
		return s.Spec.PostgreSQL, nil
	}
	return dbv1alpha1.PostgreSQL{}, fmt.Errorf("%w: %T", server.ErrUnknownServerKind, srv)
}
