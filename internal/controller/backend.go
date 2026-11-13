// Package controller reconciles Database and DatabaseRole claims with
// lib-operator-go's lifecycle.Reconcile (ADR 0002): it resolves the claim's
// server, opens the PostgreSQL admin connection through the egress policy and
// hands the framework an External that runs the SQL of package pgclient.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/egress"
	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

// Keys of the admin Secret (docs/crd-drafts.md).
const (
	AdminUsernameKey = "username"
	AdminPasswordKey = "password"
)

// ErrAdminCredentials means the admin Secret lacks the username or password.
var ErrAdminCredentials = errors.New("the admin Secret needs the keys username and password")

// ConnectFunc opens a PostgreSQL session; production uses pgclient.Connect.
type ConnectFunc = pgclient.Connector

// Config is what both reconcilers share.
type Config struct {
	// Client reads and writes claims, servers and Secrets. Its Secret reads must
	// bypass the managed-by cache, which cannot see the admin Secret.
	Client client.Client
	// APIReader looks for binding Secrets (lifecycle.Options.APIReader).
	APIReader client.Reader
	Recorder  events.EventRecorder
	// ManagedBy is the managed-by label of binding Secrets and the audit
	// controller name.
	ManagedBy             string
	OperatorNamespace     string
	DefaultDeletionPolicy common.DeletionPolicy
	Egress                egress.Policy
	Connect               ConnectFunc
	// Cache keeps the connection pool of each server between reconciles; nil
	// opens a connection for every use.
	Cache *ServerCache
	// Now is the clock of the lifecycle (rotation and grace periods); nil is
	// time.Now. Tests advance it.
	Now func() time.Time
}

// backend is a resolved server.
type backend struct {
	admin pgclient.Admin
	// pool is the server's cached admin pool; nil without a cache.
	pool *pgxpool.Pool
	// recorder reports what a claim's External cannot return as an error; nil
	// reports nothing.
	recorder events.EventRecorder
	spec     dbv1alpha1.PostgreSQL
	conn     server.Connection
}

// backend resolves ref for a claim in ns: the connection settings through
// server.Resolve (which enforces allowedNamespaces), the server's PostgreSQL
// fields, and the admin Admin whose connections dial through the egress policy
// and the server's proxy.
func (c Config) backend(ctx context.Context, ns string, ref common.ServerRef) (*backend, error) {
	cacheKey := serverKey(ns, ref)
	conn, err := server.Resolve(ctx, c.Client, ns, ref, server.WithOperatorNamespace(c.OperatorNamespace))
	if err != nil {
		// A server that is gone or no longer usable keeps no connections. A
		// namespace the server does not serve says nothing about the server
		// (other namespaces share the entry), so it is not on this list.
		if errors.Is(err, server.ErrServerNotFound) || errors.Is(err, server.ErrInvalidServer) ||
			errors.Is(err, server.ErrAdminSecretNamespaceForbidden) {
			c.Cache.Forget(cacheKey)
		}
		return nil, err
	}
	obj, err := c.fetchServer(ctx, ns, ref)
	if err != nil {
		return nil, err
	}
	spec := obj.spec
	dial, err := c.Egress.Dialer(conn.Proxy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	user, pass := conn.Admin[AdminUsernameKey], conn.Admin[AdminPasswordKey]
	if len(user) == 0 || len(pass) == 0 {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrAdminCredentials)
	}
	target := pgclient.Target{
		Host: spec.Host, Port: portOf(spec.Port), Database: spec.AdminDatabase, User: string(user), Password: string(pass),
		ServerName: conn.ServerName, CABundle: conn.CABundle, MinTLS: conn.MinTLSVersion, Dial: dial,
	}
	// Without a cache nobody would close a pool, so every session is opened
	// (and closed) on its own.
	var pool *pgxpool.Pool
	if c.Cache != nil {
		pool, err = c.Cache.Get(cacheKey, stamp(obj, target, conn.Proxy), func() (*pgxpool.Pool, error) {
			return pgclient.NewPool(target)
		})
		if err != nil {
			return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
		}
	}
	return &backend{admin: pgclient.Admin{Open: pgclient.Opener(pool, c.Connect, target)}, pool: pool, recorder: c.Recorder, spec: spec, conn: conn}, nil
}

// serverObject is the server a claim names, as much of it as the controllers
// use.
type serverObject struct {
	uid        types.UID
	generation int64
	spec       dbv1alpha1.PostgreSQL
}

// serverKey names a server in the connection cache.
func serverKey(ns string, ref common.ServerRef) string {
	if ref.Kind == dbv1alpha1.KindClusterDatabaseServer {
		ns = ""
	}
	return ref.Kind + "/" + ns + "/" + ref.Name
}

// stamp identifies what a cached pool was built from: the server object (UID
// and generation) and everything the connection takes from outside it, which
// the generation does not cover: the credentials, the CA bundle and the proxy.
// The credentials only go into a hash.
func stamp(o serverObject, t pgclient.Target, proxy *url.URL) string {
	h := sha256.New()
	for _, part := range [][]byte{
		[]byte(o.uid), []byte(strconv.FormatInt(o.generation, 10)), []byte(t.Host), []byte(strconv.Itoa(int(t.Port))),
		[]byte(t.Database), []byte(t.User), []byte(t.Password), []byte(t.ServerName), t.CABundle,
		[]byte(strconv.FormatUint(uint64(t.MinTLS), 10)), []byte(proxyText(proxy)),
	} {
		// Length-prefixed, so ("ab","c") and ("a","bc") differ.
		_, _ = h.Write(strconv.AppendInt(nil, int64(len(part)), 10))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func proxyText(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// portOf is the TCP port of a server. The schema admits 1 to 65535 and
// defaults to 5432, so anything else is a server object the API server did not
// validate, and gets the default.
func portOf(p int32) uint16 {
	if p < 1 || p > math.MaxUint16 {
		return uint16(dbv1alpha1.DefaultPort)
	}
	return uint16(p)
}

// fetchServer reads the PostgreSQL fields of the server, which
// server.Connection does not carry (ADR 0002, open issues), and its identity
// for the connection cache.
func (c Config) fetchServer(ctx context.Context, ns string, ref common.ServerRef) (serverObject, error) {
	if ref.Kind == dbv1alpha1.KindClusterDatabaseServer {
		var s dbv1alpha1.ClusterDatabaseServer
		err := c.Client.Get(ctx, client.ObjectKey{Name: ref.Name}, &s)
		return serverObject{s.UID, s.Generation, s.Spec.PostgreSQL}, err
	}
	var s dbv1alpha1.DatabaseServer
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s)
	return serverObject{s.UID, s.Generation, s.Spec.PostgreSQL}, err
}

// claimID is the claim part of the ownership marker.
func claimID(o client.Object) string {
	return fmt.Sprintf("%s/%s/%s", o.GetNamespace(), o.GetName(), o.GetUID())
}

// connLimit is spec.connectionLimit, -1 (unlimited) when unset.
func connLimit(p *int32) int32 {
	if p == nil {
		return -1
	}
	return *p
}
