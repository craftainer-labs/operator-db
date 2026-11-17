package controller

import (
	"context"

	"github.com/craftainer-labs/lib-operator-go/pkg/server"
)

// probeOnce runs ProbeCloud as probe.Reconciler does and returns the version.
// On success it also writes what was discovered to the server's status, which
// the reconciler does with the result.
func probeOnce(ctx context.Context, p Prober, srv server.CloudServer, conn server.Connection) (string, error) {
	res, err := p.ProbeCloud(ctx, srv, server.CloudConnection{Connection: conn})
	if err == nil {
		if st := serverStatusOf(srv); st != nil {
			st.Discovered = res.Discovered
		}
	}
	return res.Version, err
}
