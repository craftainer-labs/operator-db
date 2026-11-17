//go:build integration

package integration

import (
	"context"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	libserver "github.com/craftainer-labs/lib-operator-go/pkg/server"

	"github.com/craftainer-labs/operator-db/internal/controller"
)

// statusOf is what probe.CloudServer adds to server.CloudServer.
type statusOf interface{ GetServerStatus() *common.ServerStatus }

// probeVersion runs the Prober as probe.Reconciler does and returns the
// version. On success it also writes what was discovered to the server's
// status, which the reconciler does with the result. creds is the data of the
// cloud credentials Secret, which an Entra administrator's token needs.
func probeVersion(ctx context.Context, p controller.Prober, srv libserver.CloudServer, conn libserver.Connection, creds map[string][]byte) (string, error) {
	res, err := p.ProbeCloud(ctx, srv, libserver.CloudConnection{Connection: conn, Credentials: creds})
	if err == nil {
		if s, ok := srv.(statusOf); ok {
			s.GetServerStatus().Discovered = res.Discovered
		}
	}
	return res.Version, err
}
