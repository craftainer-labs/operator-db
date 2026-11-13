// Package pgclient is the PostgreSQL backend client (NR4: jackc/pgx/v5). It
// opens TLS connections with the server's CA bundle and never skips
// certificate verification, quotes every identifier with pgx.Identifier and
// every literal with Literal, and runs the SQL of the operator's two backend
// objects: databases, and the owner and login roles behind a DatabaseRole.
package pgclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/craftainer-labs/operator-db/internal/egress"
)

// ErrInvalidCA means the server's CA bundle holds no certificate.
var ErrInvalidCA = errors.New("the CA bundle holds no PEM certificate")

// Row is one result row; every column is text, a NULL is the empty string.
type Row map[string]string

// Session is one connection. All statements are complete SQL texts built with
// Ident and Literal, never with bound parameters.
type Session interface {
	Exec(ctx context.Context, sql string) error
	Query(ctx context.Context, sql string) ([]Row, error)
	Close(ctx context.Context)
}

// Target is one connection: where to go and how to trust it.
type Target struct {
	Host     string
	Port     uint16
	Database string
	User     string
	Password string
	// ServerName overrides the host used for certificate verification.
	ServerName string
	// CABundle is PEM; empty uses the system roots.
	CABundle []byte
	// MinTLS is a crypto/tls version; below TLS 1.2 is raised to 1.2.
	MinTLS uint16
	// Dial opens the TCP connection (see package egress).
	Dial egress.DialFunc
}

// tlsConfig verifies the server certificate chain and host name in every
// sslMode the API offers: verify-ca and require are verified as verify-full,
// because ADR 0002 section 13 forbids InsecureSkipVerify (a server whose
// certificate names another host sets tls.serverName).
func (t Target) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: max(t.MinTLS, tls.VersionTLS12), ServerName: t.ServerName}
	if cfg.ServerName == "" {
		cfg.ServerName = t.Host
	}
	if len(t.CABundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(t.CABundle) {
			return nil, ErrInvalidCA
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func (t Target) config() (*pgx.ConnConfig, error) {
	tlsCfg, err := t.tlsConfig()
	if err != nil {
		return nil, err
	}
	// sslmode=disable only keeps ParseConfig from building plaintext fallbacks;
	// the TLS configuration is set below and is required.
	cfg, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.Database = t.Host, t.Port, t.Database
	cfg.User, cfg.Password = t.User, t.Password
	cfg.TLSConfig, cfg.Fallbacks = tlsCfg, nil
	cfg.ConnectTimeout = 15 * time.Second
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return t.Dial(ctx, network, addr)
	}
	// Text protocol with client-side quoting needs standard_conforming_strings;
	// passwords are stored as SCRAM-SHA-256 only (ADR 0002 section 13).
	cfg.RuntimeParams["standard_conforming_strings"] = "on"
	cfg.RuntimeParams["password_encryption"] = "scram-sha-256"
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return cfg, nil
}

// Connect opens a Session.
func Connect(ctx context.Context, t Target) (Session, error) {
	cfg, err := t.config()
	if err != nil {
		return nil, err
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return conn{c}, nil
}

type conn struct{ c *pgx.Conn }

func (c conn) Exec(ctx context.Context, sql string) error {
	_, err := c.c.Exec(ctx, sql)
	return err
}

func (c conn) Query(ctx context.Context, sql string) ([]Row, error) {
	var maps []map[string]any
	rows, err := c.c.Query(ctx, sql)
	if err == nil {
		maps, err = pgx.CollectRows(rows, pgx.RowToMap)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Row, len(maps))
	for i, m := range maps {
		r := make(Row, len(m))
		for k, v := range m {
			if v != nil {
				r[k] = fmt.Sprint(v)
			}
		}
		out[i] = r
	}
	return out, nil
}

func (c conn) Close(ctx context.Context) { _ = c.c.Close(ctx) }
