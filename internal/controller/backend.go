// Package controller reconciles Database and DatabaseRole claims with
// lib-operator-go's lifecycle.Reconcile (ADR 0002): it resolves the claim's
// server, opens the PostgreSQL admin connection through the egress policy and
// hands the framework an External that runs the SQL of package pgclient.
package controller

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
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
	// EgressMode is the mode Egress was built from; the cloud clients use it
	// to decide whether endpointOverride is honoured.
	EgressMode egress.Mode
	// RDS builds the RDS client of a cloud server (NewRDS in production). It
	// must be set for a cloud server.
	RDS func(aws.Config) RDSAPI
	// SQLAdmin builds the Cloud SQL Admin client of a GCP server
	// (NewSQLAdmin in production). It must be set for a GCP server.
	SQLAdmin func(context.Context, *cloud.GCPClient) (SQLAdminAPI, error)
	// ARM builds the Azure Resource Manager client of an Azure server (NewARM
	// in production). It must be set for an Azure server.
	ARM func(*cloud.AzureClient, string) (ARMAPI, error)
	// CloudRootCAs are the roots the cloud API clients trust, nil for the
	// system's. Production leaves it nil; a test of an emulator that serves TLS
	// with its own CA sets it.
	CloudRootCAs *x509.CertPool
	Connect      ConnectFunc
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
	// host and port are where the data plane is: spec.host and spec.port, or
	// the address discovered from the cloud.
	host string
	port int32
	// cloud is set for a cloud server, with the region it was discovered in and
	// what the probe discovered (status.discovered).
	cloud      bool
	region     string
	discovered map[string]string
	// provider is the cloud of the server, "" for a self-hosted one. cs, cfg
	// and proxy are what the Cloud SQL Admin client of a GCP server is built
	// from.
	provider cloud.ProviderType
	cs       *cloudSpec
	cfg      Config
}

// sqlAdmin builds the Cloud SQL Admin client of a GCP server; done releases it.
func (b *backend) sqlAdmin(ctx context.Context) (api SQLAdminAPI, done func(), err error) {
	return b.cfg.gcpAPI(ctx, b.cs, b.conn)
}

// backend resolves ref for a claim in ns: the connection settings through
// server.ResolveCloud (which enforces allowedNamespaces and reads the admin
// Secret, if the server names one, and the cloud credentials), the server's
// PostgreSQL fields, and the admin Admin whose connections dial through the
// egress policy and the server's proxy. With adminAuthMode IAM the login is the
// Entra administrator's, with a token for each new connection.
func (c Config) backend(ctx context.Context, ns string, ref common.ServerRef) (*backend, error) {
	cacheKey := serverKey(ns, ref)
	cc, err := server.ResolveCloud(ctx, c.Client, ns, ref, server.WithOperatorNamespace(c.OperatorNamespace))
	conn := cc.Connection
	if err != nil {
		// A server that is gone or no longer usable keeps no connections. A
		// namespace the server does not serve says nothing about the server
		// (other namespaces share the entry), so it is not on this list.
		if errors.Is(err, server.ErrServerNotFound) || errors.Is(err, server.ErrInvalidServer) ||
			errors.Is(err, server.ErrAdminSecretNamespaceForbidden) || errors.Is(err, server.ErrCredentialsSecretNamespaceForbidden) {
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
	target, err := adminTarget(spec, obj.discovered, conn, dial, c.entraLogin(obj.cs, cc))
	if err != nil {
		return nil, err
	}
	// Without a cache nobody would close a pool, so every session is opened
	// (and closed) on its own.
	var pool *pgxpool.Pool
	if c.Cache != nil {
		pool, err = c.Cache.Get(cacheKey, stamp(obj, target, conn.Proxy, cc.Credentials), func() (*pgxpool.Pool, error) {
			return pgclient.NewPool(target)
		})
		if err != nil {
			return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
		}
	}
	return &backend{admin: pgclient.Admin{Open: pgclient.Opener(pool, c.Connect, target)}, pool: pool, recorder: c.Recorder, spec: spec, conn: conn,
		host: target.Host, port: int32(target.Port), cloud: obj.cloud, region: firstNonEmpty(obj.region, obj.discovered[dbv1alpha1.DiscoveredRegion]),
		discovered: obj.discovered, provider: providerOf(obj.cs), cs: obj.cs, cfg: c}, nil
}

// IAMResourceARN is the iamResourceArn of an IAM-mode login named user: the ARN
// its consumer's role must be allowed to rds-db:connect to,
// arn:aws:rds-db:<region>:<account>:dbuser:<resourceId>/<user>. The partition
// is aws (no sovereign clouds in v1alpha1). The account and the resource id are
// what the probe discovered (status.discovered); without them, or without the
// region, it is ErrIAMTargetUnknown.
func IAMResourceARN(region string, discovered map[string]string, user string) (string, error) {
	account, id := discovered[dbv1alpha1.DiscoveredAccountID], discovered[dbv1alpha1.DiscoveredResourceID]
	if region == "" || account == "" || id == "" {
		return "", ErrIAMTargetUnknown
	}
	return "arn:aws:rds-db:" + region + ":" + account + ":dbuser:" + id + "/" + user, nil
}

// adminTarget is the connection to a server's admin database: its host, port
// and admin database, the admin credentials of conn (or, with entra set, the
// Entra administrator and its token), and the TLS settings.
func adminTarget(spec dbv1alpha1.PostgreSQL, discovered map[string]string, conn server.Connection, dial egress.DialFunc, entra *entraLogin) (pgclient.Target, error) {
	host, port := dbv1alpha1.Target(spec, discovered)
	if host == "" {
		return pgclient.Target{}, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrNotDiscovered)
	}
	user, pass := string(conn.Admin[AdminUsernameKey]), string(conn.Admin[AdminPasswordKey])
	var token func(context.Context) (string, error)
	if entra != nil {
		user, pass, token = entra.user, "", entra.token
	} else if user == "" || pass == "" {
		return pgclient.Target{}, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrAdminCredentials)
	}
	// A Cloud SQL instance is reached by IP address, which its certificate does
	// not name: the TLS server name defaults to the instance's DNS name, which
	// the certificate of an instance with one carries. An explicit
	// tls.serverName wins, and so does an address set in spec.host.
	name := conn.ServerName
	if name == "" && spec.Host == "" {
		name = discovered[dbv1alpha1.DiscoveredDNSName]
	}
	return pgclient.Target{
		Host: host, Port: portOf(port), Database: spec.AdminDatabase, User: user, Password: pass, Token: token,
		ServerName: name, CABundle: conn.CABundle, MinTLS: conn.MinTLSVersion, Dial: dial,
	}, nil
}

// serverObject is the server a claim names, as much of it as the controllers
// use.
type serverObject struct {
	uid        types.UID
	generation int64
	spec       dbv1alpha1.PostgreSQL
	// discovered is status.discovered, and cloud says spec.provider is set.
	discovered map[string]string
	cloud      bool
	// region is spec.provider.aws.region.
	region string
	// cs is the provider settings, nil for a self-hosted server.
	cs *cloudSpec
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
// The credentials only go into a hash: the admin password, and the cloud
// credentials (secret), which an Entra administrator's token is fetched with.
// An Entra login is told from a password one by the token.
func stamp(o serverObject, t pgclient.Target, proxy *url.URL, secret map[string][]byte) string {
	h := sha256.New()
	login := "password"
	if t.Token != nil {
		login = "entra"
	}
	parts := [][]byte{
		[]byte(o.uid), []byte(strconv.FormatInt(o.generation, 10)), []byte(t.Host), []byte(strconv.Itoa(int(t.Port))),
		[]byte(t.Database), []byte(t.User), []byte(t.Password), []byte(t.ServerName), t.CABundle,
		[]byte(strconv.FormatUint(uint64(t.MinTLS), 10)), []byte(proxyText(proxy)), []byte(login),
	}
	for _, k := range slices.Sorted(maps.Keys(secret)) {
		parts = append(parts, []byte(k), secret[k])
	}
	for _, part := range parts {
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
		cs := cloudOf(&s)
		return serverObject{uid: s.UID, generation: s.Generation, spec: s.Spec.PostgreSQL, discovered: s.Status.Discovered,
			cloud: cs != nil, region: regionOf(cs), cs: cs}, err
	}
	var s dbv1alpha1.DatabaseServer
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s)
	cs := cloudOf(&s)
	return serverObject{uid: s.UID, generation: s.Generation, spec: s.Spec.PostgreSQL, discovered: s.Status.Discovered,
		cloud: cs != nil, region: regionOf(cs), cs: cs}, err
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

// firstNonEmpty returns a, or b when a is empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
