package controller

import (
	"context"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"

	"github.com/craftainer-labs/operator-db/internal/pgclient"
)

type cacheClock struct{ t time.Time }

func (c *cacheClock) now() time.Time { return c.t }

func poolOf(c *pgclient.Pool) func() *pgclient.Pool { return func() *pgclient.Pool { return c } }

func newPool() *pgclient.Pool {
	return pgclient.NewPool(func(context.Context, pgclient.Target) (pgclient.Session, error) { return nil, errStatement },
		pgclient.Target{}, 1, time.Minute)
}

func TestServerCacheReusesByVersion(t *testing.T) {
	clock := &cacheClock{t: time.Unix(0, 0)}
	c := NewServerCache(time.Minute)
	c.now = clock.now
	a, b := newPool(), newPool()

	if got := c.Get("k", "v1", poolOf(a)); got != a {
		t.Fatal("first Get did not build")
	}
	if got := c.Get("k", "v1", poolOf(b)); got != a || c.Len() != 1 {
		t.Error("the same version was rebuilt")
	}
	// A new version replaces the entry.
	if got := c.Get("k", "v2", poolOf(b)); got != b || c.Len() != 1 {
		t.Error("a new version was not rebuilt")
	}

	// An entry idle for longer than the idle time is dropped by the next Get.
	clock.t = clock.t.Add(2 * time.Minute)
	other := newPool()
	if got := c.Get("other", "v", poolOf(other)); got != other || c.Len() != 1 {
		t.Errorf("stale entry kept: %d entries", c.Len())
	}

	c.Forget("absent")
	c.Forget("other")
	if c.Len() != 0 {
		t.Error("Forget kept the entry")
	}
}

func TestNilServerCacheCachesNothing(t *testing.T) {
	var c *ServerCache
	p := newPool()
	if c.Get("k", "v", poolOf(p)) != p {
		t.Error("a nil cache did not build")
	}
	c.Forget("k")
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

// The connections of a server are reused until the server, its credentials or
// its CA change, and a missing server is forgotten.
func TestBackendReusesTheServersPool(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionRetain, nil)
	h.cfg.Cache = NewServerCache(time.Minute)

	use := func() {
		t.Helper()
		be, err := h.cfg.backend(ctx, h.ns, h.ref())
		h.must(err)
		s, err := be.admin.Open(ctx, "")
		h.must(err)
		s.Close(ctx)
	}
	use()
	use()
	if len(h.fake.targets) != 1 || h.cfg.Cache.Len() != 1 {
		t.Fatalf("%d connections, %d cached servers; want 1, 1", len(h.fake.targets), h.cfg.Cache.Len())
	}

	// New credentials in the admin Secret are a new version.
	var admin corev1.Secret
	h.must(env.Client.Get(ctx, client.ObjectKey{Namespace: h.ops, Name: h.server.Spec.AdminCredentialsSecretRef.Name}, &admin))
	admin.Data["password"] = []byte("rotated")
	h.must(env.Client.Update(ctx, &admin))
	use()
	if len(h.fake.targets) != 2 || h.fake.targets[1].Password != "rotated" || h.cfg.Cache.Len() != 1 {
		t.Errorf("%d connections after the credentials changed", len(h.fake.targets))
	}

	// A server that is not there is forgotten.
	h.cfg.Cache.Get("DatabaseServer/"+h.ns+"/gone", "v", func() *pgclient.Pool { return newPool() })
	gone := common.ServerRef{Kind: "DatabaseServer", Name: "gone"}
	if _, err := h.cfg.backend(ctx, h.ns, gone); err == nil || h.cfg.Cache.Len() != 1 {
		t.Errorf("missing server: %v, %d cached", err, h.cfg.Cache.Len())
	}
}
