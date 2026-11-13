package pgclient

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The settings of a server's admin pool: a reconcile uses one admin session at
// a time per statement group, so a couple of connections is plenty, and an idle
// one goes after a short time so a restarted or timing-out server costs at most
// one retried reconcile.
const (
	poolMaxConns        = 2
	poolMaxConnIdleTime = 30 * time.Second
	poolHealthCheck     = 30 * time.Second
)

// NewPool returns the connection pool of a server's admin database (t.Database
// is the admin database). It opens no connection until the first Open. Only
// the admin database is pooled: a session on a claim's database would be killed
// by DROP DATABASE ... WITH (FORCE) and could outlive the database, so those
// are opened and closed per use by Opener.
//
// pgxpool discards a connection that is closed or broken when it is released,
// pings one that sat idle before handing it out, and closes idle ones after
// poolMaxConnIdleTime.
func NewPool(t Target) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig("sslmode=disable")
	if err != nil {
		return nil, err
	}
	if err := t.apply(pc.ConnConfig); err != nil {
		return nil, err
	}
	pc.MaxConns, pc.MaxConnIdleTime, pc.HealthCheckPeriod = poolMaxConns, poolMaxConnIdleTime, poolHealthCheck
	return pgxpool.NewWithConfig(context.Background(), pc)
}

// Opener returns the OpenFunc of a server: "" is the admin database, taken from
// pool when there is one; any other database is opened with connect and closed
// when the session is. A nil pool opens the admin database with connect too.
func Opener(pool *pgxpool.Pool, connect Connector, t Target) OpenFunc {
	return func(ctx context.Context, database string) (Session, error) {
		if database == "" && pool != nil {
			c, err := pool.Acquire(ctx)
			if err != nil {
				return nil, err
			}
			return session{q: c, done: func(context.Context) { c.Release() }}, nil
		}
		target := t
		if database != "" {
			target.Database = database
		}
		return connect(ctx, target)
	}
}
