package pgclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/craftainer-labs/operator-db/internal/egress"
)

func target(t *testing.T, fs *fakeServer, ca []byte) Target {
	t.Helper()
	host, port, _ := net.SplitHostPort(fs.addr)
	p, _ := strconv.ParseUint(port, 10, 16)
	open, _ := egress.Policy{}.Dialer(nil)
	return Target{Host: host, Port: uint16(p), Database: "postgres", User: "admin", Password: "pw", CABundle: ca, MinTLS: tls.VersionTLS13, Dial: open}
}

func TestConnectAndSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ca := newTestCA(t)
	fs := startFakeServer(t, ca)

	s, err := Connect(ctx, target(t, fs, ca.caPEM))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if err := s.Exec(ctx, "CREATE ROLE x"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Query(ctx, "SELECT rows")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["a"] != "x" || rows[0]["b"] != "" || rows[1]["b"] != "z" {
		t.Errorf("rows = %v", rows)
	}
	if err := s.Exec(ctx, "FAIL"); err == nil {
		t.Error("Exec: no error for an ErrorResponse")
	}
	if _, err := s.Query(ctx, "SELECT FAIL"); err == nil {
		t.Error("Query: no error for an ErrorResponse")
	}
}

// Verification is never skipped: an untrusted certificate, and a trusted one
// for another name, both fail, whatever the sslMode.
func TestConnectVerifiesTheServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ca := newTestCA(t)
	fs := startFakeServer(t, ca)

	if _, err := Connect(ctx, target(t, fs, nil)); err == nil {
		t.Error("connected without trusting the CA")
	}
	other := newTestCA(t)
	if _, err := Connect(ctx, target(t, fs, other.caPEM)); err == nil {
		t.Error("connected with another CA")
	}
	named := target(t, fs, ca.caPEM)
	named.ServerName = "other.test"
	if _, err := Connect(ctx, named); err == nil {
		t.Error("connected although the name does not match")
	}
	named.ServerName = "pg.test"
	s, err := Connect(ctx, named)
	if err != nil {
		t.Fatalf("serverName override: %v", err)
	}
	s.Close(ctx)
}

func TestConnectErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bad := Target{Host: "127.0.0.1", Port: 1, CABundle: []byte("not pem")}
	if _, err := Connect(ctx, bad); !errors.Is(err, ErrInvalidCA) {
		t.Errorf("bad CA: %v", err)
	}
	open, _ := egress.Policy{}.Dialer(nil)
	down := Target{Host: "127.0.0.1", Port: 1, Dial: open}
	if _, err := Connect(ctx, down); err == nil {
		t.Error("connect to a closed port succeeded")
	}
	// The egress policy stops the dial after resolution.
	blocked, _ := egress.NewPolicy(egress.ModeStrict).Dialer(nil)
	down.Dial = blocked
	if _, err := Connect(ctx, down); !errors.Is(err, egress.ErrBlocked) {
		t.Errorf("blocked: %v", err)
	}
}

// ParseConfig reads PG* variables from the environment; a broken one is an
// error, not a panic.
func TestConfigRejectsABrokenEnvironment(t *testing.T) {
	t.Setenv("PGPORT", "not-a-port")
	if _, err := (Target{Host: "127.0.0.1"}).config(); err == nil {
		t.Error("no error for PGPORT=not-a-port")
	}
}

func TestTLSConfig(t *testing.T) {
	cfg, err := Target{Host: "pg.test", MinTLS: tls.VersionTLS10}.tlsConfig()
	if err != nil || cfg.MinVersion != tls.VersionTLS12 || cfg.ServerName != "pg.test" || cfg.InsecureSkipVerify {
		t.Errorf("cfg = %+v, %v", cfg, err)
	}
	cfg, _ = Target{Host: "pg.test", ServerName: "x", MinTLS: tls.VersionTLS13}.tlsConfig()
	if cfg.MinVersion != tls.VersionTLS13 || cfg.ServerName != "x" {
		t.Errorf("cfg = %+v", cfg)
	}
}
