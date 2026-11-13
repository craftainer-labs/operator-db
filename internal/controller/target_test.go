package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
	"github.com/craftainer-labs/operator-db/internal/egress"
)

// The server's settings reach the connection: its port, admin database, CA
// bundle (user decision 3: passed through, with no way to skip verification),
// server name, TLS floor, and the egress policy on the dialer (decision 2).
func TestTargetFollowsTheServer(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-ca", Namespace: h.ops},
		Data:       map[string][]byte{"root.pem": []byte("PEM BUNDLE")},
	}
	h.must(env.Client.Create(context.Background(), ca))
	h.get(h.server)
	h.server.Spec.Port = 6432
	h.server.Spec.AdminDatabase = "adm"
	h.server.Spec.SSLMode = dbv1alpha1.SSLModeRequire
	h.server.Spec.ProxyURL = "http://proxy.example.test:3128"
	h.server.Spec.TLS = &common.TLS{
		CABundle:   &common.CABundle{SecretRef: &common.KeyReference{Name: "pg-ca", Key: "root.pem"}},
		ServerName: "pg.internal", MinVersion: common.TLS13,
	}
	h.must(env.Client.Update(context.Background(), h.server))

	r := &DatabaseReconciler{h.cfg}
	h.reconcileOK(r, h.database("target", nil))
	if len(h.fake.targets) == 0 {
		t.Fatal("no connection")
	}
	tg := h.fake.targets[0]
	if tg.Port != 6432 || tg.Database != "adm" || tg.ServerName != "pg.internal" || string(tg.CABundle) != "PEM BUNDLE" ||
		tg.MinTLS != tls.VersionTLS13 {
		t.Errorf("target = %+v", tg)
	}
	// The destination of a proxied dial is checked before the proxy is used.
	if _, err := tg.Dial(context.Background(), "tcp", "10.1.2.3:6432"); !errors.Is(err, egress.ErrBlocked) {
		t.Errorf("a private destination was not blocked: %v", err)
	}
}

func TestPortOf(t *testing.T) {
	for in, want := range map[int32]uint16{0: 5432, 1: 1, 6432: 6432, 65535: 65535, -1: 5432, 65536: 5432, 1 << 20: 5432} {
		if got := portOf(in); got != want {
			t.Errorf("portOf(%d) = %d, want %d", in, got, want)
		}
	}
}
