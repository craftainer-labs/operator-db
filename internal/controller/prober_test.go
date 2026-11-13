package controller

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/egress"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

func versionAnswer(sql string) []pgclient.Row {
	if strings.Contains(sql, "SHOW server_version") {
		return []pgclient.Row{{"server_version": "18.1 (Debian 18.1-1)"}}
	}
	return nil
}

func probeConn() server.Connection {
	return server.Connection{
		Admin:    map[string][]byte{AdminUsernameKey: []byte("admin"), AdminPasswordKey: []byte("s3cret-pw")},
		CABundle: []byte("ca"), ServerName: "pg.internal", MinTLSVersion: 0x0304,
	}
}

// Both kinds are probed with the admin credentials, TLS settings and database
// of the server, and the version is read from the admin database.
func TestProberReadsTheVersionOfBothKinds(t *testing.T) {
	cluster := &dbv1alpha1.ClusterDatabaseServer{}
	cluster.Spec.Host, cluster.Spec.Port, cluster.Spec.AdminDatabase = "pg.example.test", 6432, "ops"
	namespaced := &dbv1alpha1.DatabaseServer{}
	namespaced.Spec.Host = "team.example.test"

	for name, srv := range map[string]server.Server{"cluster": cluster, "namespaced": namespaced} {
		fake := &pgFake{answer: versionAnswer}
		p := Prober{Config: Config{Egress: egress.NewPolicy(egress.ModeStrict), Connect: fake.connect}}
		got, err := p.Probe(context.Background(), srv, probeConn())
		if err != nil || got != "18.1 (Debian 18.1-1)" {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
		if len(fake.targets) != 1 {
			t.Fatalf("%s: %d connections", name, len(fake.targets))
		}
		tg := fake.targets[0]
		if tg.User != "admin" || tg.Password != "s3cret-pw" || tg.ServerName != "pg.internal" || string(tg.CABundle) != "ca" || tg.MinTLS != 0x0304 || tg.Dial == nil {
			t.Errorf("%s: target = %+v", name, tg)
		}
		fake.want(t, "SHOW server_version")
	}
	cluster.Spec.Port = 0
	fake := &pgFake{answer: versionAnswer}
	_, _ = Prober{Config: Config{Egress: egress.NewPolicy(egress.ModeStrict), Connect: fake.connect}}.Probe(context.Background(), cluster, probeConn())
	if fake.targets[0].Port != uint16(dbv1alpha1.DefaultPort) {
		t.Errorf("default port: %+v", fake.targets[0])
	}
}

// Whatever fails, the error carries no credential.
func TestProberFailuresLeakNothing(t *testing.T) {
	srv := &dbv1alpha1.ClusterDatabaseServer{ObjectMeta: metav1.ObjectMeta{Name: "pg"}}
	srv.Spec.Host = "pg.example.test"
	cfg := func(f *pgFake) Config {
		return Config{Egress: egress.NewPolicy(egress.ModeStrict), Connect: f.connect}
	}
	ctx := context.Background()

	// An unreachable server.
	f := &pgFake{connErr: errors.New("connect to s3cret-pw failed")}
	_, err := Prober{Config: cfg(f)}.Probe(ctx, srv, probeConn())
	if !errors.Is(err, pgclient.ErrConnFailed) || strings.Contains(err.Error(), "s3cret-pw") {
		t.Errorf("unreachable: %v", err)
	}

	// A failed query.
	f = &pgFake{queryFailOn: "server_version", queryErr: errStatement}
	if _, err = (Prober{Config: cfg(f)}).Probe(ctx, srv, probeConn()); !errors.Is(err, errStatement) {
		t.Errorf("query: %v", err)
	}

	// Missing credentials.
	conn := probeConn()
	delete(conn.Admin, AdminPasswordKey)
	if _, err = (Prober{Config: cfg(&pgFake{})}).Probe(ctx, srv, conn); !errors.Is(err, ErrAdminCredentials) || !errors.Is(err, server.ErrInvalidServer) {
		t.Errorf("credentials: %v", err)
	}

	// A proxy the egress policy cannot dial through.
	conn = probeConn()
	conn.Proxy = &url.URL{Scheme: "ftp", Host: "proxy.example.test"}
	if _, err = (Prober{Config: cfg(&pgFake{})}).Probe(ctx, srv, conn); !errors.Is(err, server.ErrInvalidServer) {
		t.Errorf("proxy: %v", err)
	}

	// A server kind of another operator.
	if _, err = (Prober{Config: cfg(&pgFake{})}).Probe(ctx, otherServer{}, probeConn()); !errors.Is(err, server.ErrUnknownServerKind) {
		t.Errorf("kind: %v", err)
	}
}

// otherServer is a server.Server this operator does not own.
type otherServer struct{ server.Server }
