package controller

import (
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ServerIdleTimeout is how long the operator keeps the connections of a server
// nobody has reconciled against.
const ServerIdleTimeout = 10 * time.Minute

// ServerCache keeps one connection pool per server between reconciles, so the
// admin connections are reused instead of opened for every reconcile. An entry
// is valid for one version, the stamp: the server object (UID and generation)
// and everything its connection is built from (credentials, CA, proxy), so it
// is replaced as soon as any of them changes. It is dropped when the server is
// gone (Forget) or has not been used for a while. A nil *ServerCache caches
// nothing.
type ServerCache struct {
	mu      sync.Mutex
	idle    time.Duration
	now     func() time.Time
	entries map[string]*serverEntry
}

type serverEntry struct {
	version string
	pool    *pgxpool.Pool
	used    time.Time
}

// NewServerCache returns a cache that drops an entry idle for longer than idle.
func NewServerCache(idle time.Duration) *ServerCache {
	return &ServerCache{idle: idle, now: time.Now, entries: map[string]*serverEntry{}}
}

// Get returns the pool cached for key if it was built for version, else builds
// one with build, replaces the entry and closes the old pool. A failed build
// leaves the cache as it was.
func (c *ServerCache) Get(key, version string, build func() (*pgxpool.Pool, error)) (*pgxpool.Pool, error) {
	if c == nil {
		return build()
	}
	c.mu.Lock()
	now := c.now()
	var stale []*pgxpool.Pool
	for k, e := range c.entries {
		if now.Sub(e.used) > c.idle {
			stale = append(stale, e.pool)
			delete(c.entries, k)
		}
	}
	e, ok := c.entries[key]
	if ok && e.version == version {
		e.used = now
		c.mu.Unlock()
		closePools(stale)
		return e.pool, nil
	}
	pool, err := build()
	if err != nil {
		c.mu.Unlock()
		closePools(stale)
		return nil, err
	}
	if ok {
		stale = append(stale, e.pool)
	}
	e = &serverEntry{version: version, pool: pool, used: now}
	c.entries[key] = e
	c.mu.Unlock()
	closePools(stale)
	return pool, nil
}

// Forget drops the entry of a server that no longer exists.
func (c *ServerCache) Forget(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	e, ok := c.entries[key]
	delete(c.entries, key)
	c.mu.Unlock()
	if ok {
		closePools([]*pgxpool.Pool{e.pool})
	}
}

// Len is the number of cached servers.
func (c *ServerCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// closePools closes pools in the background: pgxpool's Close refuses new
// acquires at once but waits for the connections still on loan, which a
// reconcile in flight may hold for as long as its statements run.
func closePools(pools []*pgxpool.Pool) {
	for _, p := range pools {
		go p.Close()
	}
}
