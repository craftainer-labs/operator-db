package pgclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/craftainer-labs/operator-db/internal/egress"
)

func TestPoolReusesTheAdminConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ca := newTestCA(t)
	fs := startFakeServer(t, ca)
	tgt := target(t, fs, ca.caPEM)

	pool, err := NewPool(tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := pool.Config()
	if cfg.MaxConns != poolMaxConns || cfg.MaxConnIdleTime != poolMaxConnIdleTime || cfg.HealthCheckPeriod != poolHealthCheck {
		t.Errorf("pool settings = %d, %v, %v", cfg.MaxConns, cfg.MaxConnIdleTime, cfg.HealthCheckPeriod)
	}
	if pool.Stat().TotalConns() != 0 {
		t.Error("NewPool connected")
	}

	connects := 0
	open := Opener(pool, func(ctx context.Context, t Target) (Session, error) {
		connects++
		return Connect(ctx, t)
	}, tgt)

	s, err := open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "CREATE ROLE x"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Query(ctx, "SELECT rows")
	if err != nil || len(rows) != 2 || rows[0]["b"] != "" || rows[1]["b"] != "z" {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	s.Close(ctx)
	if pool.Stat().IdleConns() != 1 {
		t.Errorf("idle = %d after Close, want 1", pool.Stat().IdleConns())
	}
	s, err = open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Close(ctx)
	if pool.Stat().TotalConns() != 1 || pool.Stat().NewConnsCount() != 1 || connects != 0 {
		t.Errorf("%d connections opened, %d direct", pool.Stat().NewConnsCount(), connects)
	}
}

// A claim's database is never pooled: it is opened with the connector and
// closed with the session.
func TestOpenerDoesNotPoolClaimDatabases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ca := newTestCA(t)
	fs := startFakeServer(t, ca)
	tgt := target(t, fs, ca.caPEM)
	pool, err := NewPool(tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var got []string
	connect := func(ctx context.Context, t Target) (Session, error) {
		got = append(got, t.Database)
		return Connect(ctx, t)
	}
	s, err := Opener(pool, connect, tgt)(ctx, "team_db")
	if err != nil {
		t.Fatal(err)
	}
	s.Close(ctx)
	if len(got) != 1 || got[0] != "team_db" || pool.Stat().TotalConns() != 0 {
		t.Errorf("connected to %v, %d pooled", got, pool.Stat().TotalConns())
	}

	// Without a pool the admin database is opened with the connector as well,
	// and the target keeps its own database.
	if s, err = Opener(nil, connect, tgt)(ctx, ""); err != nil {
		t.Fatal(err)
	}
	s.Close(ctx)
	if len(got) != 2 || got[1] != "postgres" {
		t.Errorf("connected to %v", got)
	}
}

func TestOpenerErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	blocked, _ := egress.NewPolicy(egress.ModeStrict).Dialer(nil)
	down := Target{Host: "127.0.0.1", Port: 1, Dial: blocked}
	pool, err := NewPool(down)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := Opener(pool, Connect, down)(ctx, ""); !errors.Is(err, egress.ErrBlocked) {
		t.Errorf("pooled connect: %v", err)
	}
	if _, err := Opener(pool, Connect, down)(ctx, "x"); !errors.Is(err, egress.ErrBlocked) {
		t.Errorf("direct connect: %v", err)
	}
}

func TestNewPoolErrors(t *testing.T) {
	if _, err := NewPool(Target{Host: "127.0.0.1", CABundle: []byte("not pem")}); !errors.Is(err, ErrInvalidCA) {
		t.Errorf("bad CA: %v", err)
	}
	t.Setenv("PGPORT", "not-a-port")
	if _, err := NewPool(Target{Host: "127.0.0.1"}); err == nil {
		t.Error("no error for PGPORT=not-a-port")
	}
}
