package pgclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// testCA is a CA and a server certificate for 127.0.0.1.
type testCA struct {
	caPEM  []byte
	server tls.Certificate
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "pg"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"pg.test"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		server: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
	}
}

// fakeServer speaks just enough of the PostgreSQL wire protocol over TLS for
// Connect and the Session adapter: trust authentication, and simple queries
// that answer by their text. "SELECT rows" returns two rows, one with a NULL;
// text containing "FAIL" is an error; anything else completes.
type fakeServer struct {
	addr    string
	queries chan string
}

func startFakeServer(t *testing.T, ca testCA) *fakeServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{addr: l.Addr().String(), queries: make(chan string, 16)}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go fs.serve(c, ca)
		}
	}()
	return fs
}

func (fs *fakeServer) serve(c net.Conn, ca testCA) {
	defer func() { _ = c.Close() }()
	b := pgproto3.NewBackend(c, c)
	msg, err := b.ReceiveStartupMessage()
	if _, ok := msg.(*pgproto3.SSLRequest); err != nil || !ok {
		return
	}
	if _, err := c.Write([]byte{'S'}); err != nil {
		return
	}
	tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{ca.server}, MinVersion: tls.VersionTLS12})
	b = pgproto3.NewBackend(tc, tc)
	if _, err := b.ReceiveStartupMessage(); err != nil {
		return
	}
	b.Send(&pgproto3.AuthenticationOk{})
	b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if b.Flush() != nil {
		return
	}
	for {
		m, err := b.Receive()
		if err != nil {
			return
		}
		q, ok := m.(*pgproto3.Query)
		if !ok {
			return // Terminate
		}
		select {
		case fs.queries <- q.String:
		default:
		}
		switch {
		case strings.Contains(q.String, "FAIL"):
			b.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "XX000", Message: "boom"})
		case strings.Contains(q.String, "rows"):
			b.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
				{Name: []byte("a"), DataTypeOID: 25}, {Name: []byte("b"), DataTypeOID: 25},
			}})
			b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("x"), nil}})
			b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("y"), []byte("z")}})
			b.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 2")})
		default:
			b.Send(&pgproto3.CommandComplete{CommandTag: []byte("OK")})
		}
		b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if b.Flush() != nil {
			return
		}
	}
}
