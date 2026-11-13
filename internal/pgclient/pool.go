package pgclient

import (
	"context"
	"sync"
	"time"
)

// Pool keeps idle admin-database sessions of one server between reconciles, so
// a reconcile does not open (TCP, TLS, SCRAM) a connection for every statement
// group. Only the server's admin database is pooled: a session on a claim's
// database would be killed by DROP DATABASE ... WITH (FORCE) and could outlive
// the database, so those are opened and closed per use.
//
// A session that returned an error is closed instead of kept, and an idle one
// is dropped after the idle time, so a server that restarted or timed
// connections out costs at most one failed reconcile.
type Pool struct {
	connect func(context.Context, Target) (Session, error)
	target  Target
	maxIdle int
	ttl     time.Duration
	now     func() time.Time

	mu     sync.Mutex
	idle   []idleSession
	closed bool
}

type idleSession struct {
	s     Session
	since time.Time
}

// NewPool returns a Pool that connects with connect to target (whose Database
// is the admin database), keeps at most maxIdle idle sessions for at most ttl
// each. maxIdle 0 keeps none: every session is closed on release.
func NewPool(connect func(context.Context, Target) (Session, error), target Target, maxIdle int, ttl time.Duration) *Pool {
	return &Pool{connect: connect, target: target, maxIdle: maxIdle, ttl: ttl, now: time.Now}
}

// Open returns a session on database; "" is the admin database, the only one
// that is pooled. Close on the session returns it to the pool.
func (p *Pool) Open(ctx context.Context, database string) (Session, error) {
	if database != "" {
		t := p.target
		t.Database = database
		return p.connect(ctx, t)
	}
	var stale []Session
	var found Session
	p.mu.Lock()
	now := p.now()
	// The idle sessions are ordered by the time they were released, so the
	// expired ones are a prefix; the newest of the rest is reused.
	for len(p.idle) > 0 && now.Sub(p.idle[0].since) > p.ttl {
		stale = append(stale, p.idle[0].s)
		p.idle = p.idle[1:]
	}
	if n := len(p.idle); n > 0 {
		found = p.idle[n-1].s
		p.idle = p.idle[:n-1]
	}
	p.mu.Unlock()
	for _, s := range stale {
		s.Close(ctx)
	}
	if found != nil {
		return &pooled{Session: found, p: p}, nil
	}
	s, err := p.connect(ctx, p.target)
	if err != nil {
		return nil, err
	}
	return &pooled{Session: s, p: p}, nil
}

// Close ends the idle sessions and makes the sessions still in use close when
// they are released.
func (p *Pool) Close(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, i := range idle {
		i.s.Close(ctx)
	}
}

// Idle is the number of idle sessions.
func (p *Pool) Idle() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

func (p *Pool) release(ctx context.Context, s *pooled) {
	p.mu.Lock()
	if !s.failed && !p.closed && len(p.idle) < p.maxIdle {
		p.idle = append(p.idle, idleSession{s: s.Session, since: p.now()})
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	s.Session.Close(ctx)
}

// pooled is a session on loan from a Pool. One reconcile goroutine uses it at a
// time, so failed needs no lock.
type pooled struct {
	Session
	p      *Pool
	failed bool
}

func (s *pooled) Exec(ctx context.Context, sql string) error {
	err := s.Session.Exec(ctx, sql)
	s.failed = s.failed || err != nil
	return err
}

func (s *pooled) Query(ctx context.Context, sql string) ([]Row, error) {
	rows, err := s.Session.Query(ctx, sql)
	s.failed = s.failed || err != nil
	return rows, err
}

func (s *pooled) Close(ctx context.Context) { s.p.release(ctx, s) }
