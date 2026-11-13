package controller

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

type cacheClock struct{ t time.Time }

func (c *cacheClock) now() time.Time { return c.t }

// newPool is a pool that opens no connection until it is used; if it is, the
// dial fails.
func newPool() *pgxpool.Pool {
	p, err := pgclient.NewPool(pgclient.Target{
		Host: "pg.example.test", Port: 5432,
		Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errStatement },
	})
	if err != nil {
		panic(err)
	}
	return p
}

func poolOf(p *pgxpool.Pool) func() (*pgxpool.Pool, error) {
	return func() (*pgxpool.Pool, error) { return p, nil }
}

func get(t *testing.T, c *ServerCache, key, version string, p *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	got, err := c.Get(key, version, poolOf(p))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// isClosed reports whether the pool refuses connections because it was
// closed (the close runs in the background), not because the dial fails.
func isClosed(p *pgxpool.Pool) bool {
	for range 200 {
		if _, err := p.Acquire(context.Background()); errors.Is(err, puddle.ErrClosedPool) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// stillOpen reports whether the pool has not refused a connection for being
// closed (the dial may fail).
func stillOpen(p *pgxpool.Pool) bool {
	_, err := p.Acquire(context.Background())
	return !errors.Is(err, puddle.ErrClosedPool)
}

func TestServerCacheReusesByVersion(t *testing.T) {
	clock := &cacheClock{t: time.Unix(0, 0)}
	c := NewServerCache(time.Minute)
	c.now = clock.now
	a, b := newPool(), newPool()

	if got := get(t, c, "k", "v1", a); got != a {
		t.Fatal("first Get did not build")
	}
	if got := get(t, c, "k", "v1", b); got != a || c.Len() != 1 {
		t.Error("the same version was rebuilt")
	}
	if !stillOpen(a) {
		t.Error("the pool in use was closed")
	}
	// A new version replaces the entry and closes the old pool.
	if got := get(t, c, "k", "v2", b); got != b || c.Len() != 1 {
		t.Error("a new version was not rebuilt")
	}
	if !isClosed(a) {
		t.Error("the replaced pool was not closed")
	}

	// An entry idle for longer than the idle time is dropped, and its pool
	// closed, by the next Get.
	clock.t = clock.t.Add(2 * time.Minute)
	other := newPool()
	if got := get(t, c, "other", "v", other); got != other || c.Len() != 1 {
		t.Errorf("stale entry kept: %d entries", c.Len())
	}
	if !isClosed(b) {
		t.Error("the idle pool was not closed")
	}

	c.Forget("absent")
	c.Forget("other")
	if c.Len() != 0 || !isClosed(other) {
		t.Error("Forget kept the entry or its pool")
	}
}

// A pool that cannot be built is an error and changes nothing.
func TestServerCacheBuildFailure(t *testing.T) {
	c := NewServerCache(time.Minute)
	a := newPool()
	get(t, c, "k", "v1", a)
	if _, err := c.Get("k", "v2", func() (*pgxpool.Pool, error) { return nil, errStatement }); !errors.Is(err, errStatement) {
		t.Errorf("build failure: %v", err)
	}
	if got := get(t, c, "k", "v1", newPool()); got != a || !stillOpen(a) {
		t.Error("a failed build dropped the cached pool")
	}
	c.Forget("k")
}

func TestNilServerCacheCachesNothing(t *testing.T) {
	var c *ServerCache
	p := newPool()
	if got := get(t, c, "k", "v", p); got != p {
		t.Error("a nil cache did not build")
	}
	c.Forget("k")
	p.Close()
}

func TestStampCoversEverythingTheConnectionIsBuiltFrom(t *testing.T) {
	base := serverObject{uid: "uid", generation: 1}
	target := pgclient.Target{
		Host: "h", Port: 5432, Database: "postgres", User: "u", Password: "p", ServerName: "n",
		CABundle: []byte("ca"), MinTLS: 771,
	}
	proxy, _ := url.Parse("http://proxy:3128")
	ref := stamp(base, target, nil)
	if stamp(base, target, nil) != ref {
		t.Fatal("stamp is not deterministic")
	}
	variants := map[string]string{
		"uid":        stamp(serverObject{uid: "other", generation: 1}, target, nil),
		"generation": stamp(serverObject{uid: "uid", generation: 2}, target, nil),
		"proxy":      stamp(base, target, proxy),
	}
	mutate := func(name string, f func(*pgclient.Target)) {
		t := target
		f(&t)
		variants[name] = stamp(base, t, nil)
	}
	mutate("host", func(t *pgclient.Target) { t.Host = "h2" })
	mutate("port", func(t *pgclient.Target) { t.Port = 6432 })
	mutate("database", func(t *pgclient.Target) { t.Database = "d" })
	mutate("user", func(t *pgclient.Target) { t.User = "u2" })
	mutate("password", func(t *pgclient.Target) { t.Password = "p2" })
	mutate("servername", func(t *pgclient.Target) { t.ServerName = "n2" })
	mutate("ca", func(t *pgclient.Target) { t.CABundle = []byte("ca2") })
	mutate("mintls", func(t *pgclient.Target) { t.MinTLS = 772 })
	// Length prefixes keep neighbouring fields apart.
	mutate("split", func(t *pgclient.Target) { t.User, t.Password = "up", "" })
	seen := map[string]string{ref: "base"}
	for name, v := range variants {
		if prev, dup := seen[v]; dup {
			t.Errorf("%s has the same stamp as %s", name, prev)
		}
		seen[v] = name
	}
}

// The pool of a server is reused until the server, its credentials or its CA
// change, and a missing server is forgotten.
func TestBackendReusesTheServersPool(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionRetain, nil)
	h.cfg.Cache = NewServerCache(time.Minute)

	use := func() *pgxpool.Pool {
		t.Helper()
		be, err := h.cfg.backend(ctx, h.ns, h.ref())
		h.must(err)
		if be.pool == nil {
			t.Fatal("no pool with a cache")
		}
		return be.pool
	}
	first := use()
	if use() != first || h.cfg.Cache.Len() != 1 {
		t.Fatalf("the pool was rebuilt: %d cached servers", h.cfg.Cache.Len())
	}

	// New credentials in the admin Secret are a new version.
	var admin corev1.Secret
	h.must(env.Client.Get(ctx, client.ObjectKey{Namespace: h.ops, Name: h.server.Spec.AdminCredentialsSecretRef.Name}, &admin))
	admin.Data["password"] = []byte("rotated")
	h.must(env.Client.Update(ctx, &admin))
	second := use()
	if second == first || h.cfg.Cache.Len() != 1 || !isClosed(first) {
		t.Errorf("the credentials changed but the pool was kept (%d cached)", h.cfg.Cache.Len())
	}
	if second.Config().ConnConfig.Password != "rotated" {
		t.Error("the new pool does not use the new password")
	}

	// A server that is not there is forgotten.
	gone := newPool()
	get(t, h.cfg.Cache, "DatabaseServer/"+h.ns+"/gone", "v", gone)
	missing := common.ServerRef{Kind: "DatabaseServer", Name: "gone"}
	if _, err := h.cfg.backend(ctx, h.ns, missing); err == nil || h.cfg.Cache.Len() != 1 || !isClosed(gone) {
		t.Errorf("missing server: %v, %d cached", err, h.cfg.Cache.Len())
	}
}

// A CA bundle that holds no certificate makes the server invalid; without a
// cache nothing is built before the connection.
func TestBackendRejectsAnInvalidCABundle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionRetain, nil)
	h.must(env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-ca", Namespace: h.ops},
		Data:       map[string][]byte{"root.pem": []byte("not a certificate")},
	}))
	h.get(h.server)
	h.server.Spec.TLS = &common.TLS{CABundle: &common.CABundle{SecretRef: &common.KeyReference{Name: "bad-ca", Key: "root.pem"}}}
	h.must(env.Client.Update(ctx, h.server))

	if be, err := h.cfg.backend(ctx, h.ns, h.ref()); err != nil || be.pool != nil {
		t.Fatalf("without a cache: %v", err)
	}
	h.cfg.Cache = NewServerCache(time.Minute)
	_, err := h.cfg.backend(ctx, h.ns, h.ref())
	if !errors.Is(err, server.ErrInvalidServer) || !errors.Is(err, pgclient.ErrInvalidCA) || h.cfg.Cache.Len() != 0 {
		t.Errorf("with a cache: %v, %d cached", err, h.cfg.Cache.Len())
	}
}
