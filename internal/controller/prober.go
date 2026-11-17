package controller

import (
	"context"
	"fmt"

	"github.com/craftainer-labs/lib-operator-go/pkg/probe"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// Prober is the probe.CloudProber of both server kinds: it resolves a cloud
// server's address, opens an admin session through the egress policy and the
// server's proxy and TLS settings (the same target the claims' reconcilers use)
// and reads server_version. The caller (probe.Reconciler) bounds it by
// Options.Timeout and shows only that it failed; the error text names a
// category of failure, never a credential.
type Prober struct{ Config Config }

var _ probe.CloudProber = Prober{}

// ProbeCloud returns the server_version of srv and what it discovered in the
// cloud (empty for a self-hosted server). The session is opened for the probe
// and closed with it, not taken from the claims' pool cache, so a probe of a
// server nobody claims leaves nothing behind. With adminAuthMode IAM it logs in
// as the Entra administrator with a token.
func (p Prober) ProbeCloud(ctx context.Context, srv server.CloudServer, cc server.CloudConnection) (probe.CloudResult, error) {
	conn := cc.Connection
	spec, err := postgresOf(srv)
	if err != nil {
		return probe.CloudResult{}, err
	}
	dial, err := p.Config.Egress.Dialer(conn.Proxy)
	if err != nil {
		return probe.CloudResult{}, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	cs := cloudOf(srv)
	disc, derr := p.Config.discover(ctx, cs, conn)
	record(srv, cs, disc, derr)
	if derr != nil {
		return probe.CloudResult{}, derr
	}
	target, err := adminTarget(spec, disc, conn, dial, p.Config.entraLogin(cs, cc))
	if err != nil {
		return probe.CloudResult{}, err
	}
	admin := pgclient.Admin{Open: pgclient.Opener(nil, p.Config.Connect, target)}
	version, err := admin.ServerVersion(ctx)
	if err != nil {
		return probe.CloudResult{}, err
	}
	return probe.CloudResult{Version: version, Discovered: disc}, nil
}

// postgresOf returns the PostgreSQL fields of a server kind of this operator.
func postgresOf(srv server.CloudServer) (dbv1alpha1.PostgreSQL, error) {
	switch s := srv.(type) {
	case *dbv1alpha1.ClusterDatabaseServer:
		return s.Spec.PostgreSQL, nil
	case *dbv1alpha1.DatabaseServer:
		return s.Spec.PostgreSQL, nil
	}
	return dbv1alpha1.PostgreSQL{}, fmt.Errorf("%w: %T", server.ErrUnknownServerKind, srv)
}
