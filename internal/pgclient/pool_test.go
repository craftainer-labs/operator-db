package pgclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

type poolSession struct {
	closed bool
	fail   bool
}

func (s *poolSession) Exec(context.Context, string) error {
	if s.fail {
		return errBoom
	}
	return nil
}

func (s *poolSession) Query(context.Context, string) ([]Row, error) {
	if s.fail {
		return nil, errBoom
	}
	return nil, nil
}

func (s *poolSession) Close(context.Context) { s.closed = true }

type poolHarness struct {
	pool     *Pool
	sessions []*poolSession
	targets  []Target
	err      error
	clock    time.Time
}

func newPoolHarness(maxIdle int) *poolHarness {
	h := &poolHarness{clock: time.Unix(1000, 0)}
	connect := func(_ context.Context, t Target) (Session, error) {
		if h.err != nil {
			return nil, h.err
		}
		s := &poolSession{}
		h.sessions = append(h.sessions, s)
		h.targets = append(h.targets, t)
		return s, nil
	}
	h.pool = NewPool(connect, Target{Database: "postgres"}, maxIdle, time.Minute)
	h.pool.now = func() time.Time { return h.clock }
	return h
}

func TestPoolReusesTheAdminSession(t *testing.T) {
	ctx := context.Background()
	h := newPoolHarness(2)
	s, err := h.pool.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	s.Close(ctx)
	if h.pool.Idle() != 1 || h.sessions[0].closed {
		t.Fatalf("idle %d, closed %v", h.pool.Idle(), h.sessions[0].closed)
	}
	s, _ = h.pool.Open(ctx, "")
	s.Close(ctx)
	if len(h.sessions) != 1 || h.targets[0].Database != "postgres" {
		t.Errorf("connected %d times, to %q", len(h.sessions), h.targets[0].Database)
	}

	// Beyond maxIdle a session is closed.
	a, _ := h.pool.Open(ctx, "")
	b, _ := h.pool.Open(ctx, "")
	c, _ := h.pool.Open(ctx, "")
	a.Close(ctx)
	b.Close(ctx)
	c.Close(ctx)
	if h.pool.Idle() != 2 || !h.sessions[2].closed {
		t.Errorf("idle %d, third closed %v", h.pool.Idle(), h.sessions[2].closed)
	}
}

func TestPoolDoesNotPoolClaimDatabases(t *testing.T) {
	ctx := context.Background()
	h := newPoolHarness(2)
	s, err := h.pool.Open(ctx, "team_db")
	if err != nil {
		t.Fatal(err)
	}
	if h.targets[0].Database != "team_db" {
		t.Errorf("database = %q", h.targets[0].Database)
	}
	s.Close(ctx)
	if !h.sessions[0].closed || h.pool.Idle() != 0 {
		t.Error("a claim database session was kept")
	}
	h.err = errBoom
	if _, err := h.pool.Open(ctx, "team_db"); !errors.Is(err, errBoom) {
		t.Errorf("connect failure: %v", err)
	}
	if _, err := h.pool.Open(ctx, ""); !errors.Is(err, errBoom) {
		t.Errorf("admin connect failure: %v", err)
	}
}

func TestPoolDiscardsFailedAndExpiredSessions(t *testing.T) {
	ctx := context.Background()
	h := newPoolHarness(2)
	s, _ := h.pool.Open(ctx, "")
	h.sessions[0].fail = true
	if err := s.Exec(ctx, "x"); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	s.Close(ctx)
	if !h.sessions[0].closed || h.pool.Idle() != 0 {
		t.Error("a failed session was kept")
	}
	s, _ = h.pool.Open(ctx, "")
	h.sessions[1].fail = true
	if _, err := s.Query(ctx, "x"); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	s.Close(ctx)
	if !h.sessions[1].closed {
		t.Error("a session that failed a query was kept")
	}

	// Two idle sessions expire; Open then connects anew.
	a, _ := h.pool.Open(ctx, "")
	b, _ := h.pool.Open(ctx, "")
	a.Close(ctx)
	b.Close(ctx)
	n := len(h.sessions)
	h.clock = h.clock.Add(2 * time.Minute)
	s, err := h.pool.Open(ctx, "")
	if err != nil || len(h.sessions) != n+1 || !h.sessions[n-1].closed || !h.sessions[n-2].closed {
		t.Errorf("expired sessions were reused: %v, %d sessions", err, len(h.sessions))
	}
	s.Close(ctx)

	// A fresh one under an expired one: the expired one is closed, the fresh
	// one is used.
	old, _ := h.pool.Open(ctx, "")
	fresh, _ := h.pool.Open(ctx, "")
	old.Close(ctx)
	h.clock = h.clock.Add(90 * time.Second)
	fresh.Close(ctx)
	n = len(h.sessions)
	got, _ := h.pool.Open(ctx, "")
	if len(h.sessions) != n {
		t.Error("the fresh idle session was not reused")
	}
	got.Close(ctx)
}

func TestPoolClose(t *testing.T) {
	ctx := context.Background()
	h := newPoolHarness(2)
	a, _ := h.pool.Open(ctx, "")
	b, _ := h.pool.Open(ctx, "")
	a.Close(ctx)
	h.pool.Close(ctx)
	if !h.sessions[0].closed || h.pool.Idle() != 0 {
		t.Error("Close kept an idle session")
	}
	b.Close(ctx)
	if !h.sessions[1].closed {
		t.Error("a session released after Close was kept")
	}

	// maxIdle 0 keeps nothing.
	h = newPoolHarness(0)
	s, _ := h.pool.Open(ctx, "")
	s.Close(ctx)
	if !h.sessions[0].closed {
		t.Error("maxIdle 0 kept a session")
	}
}
