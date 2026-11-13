// Package egress limits where the operator's backend connections may go (user
// decision 2, 2026-11-03). A namespaced DatabaseServer lets a tenant aim the
// operator at any host, so every connection is checked after the host name has
// been resolved, at connect time, against a deny list chosen by an operator
// environment variable. It also dials through the server's proxy, which may
// not carry credentials (user decision 1).
package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// EnvMode names the environment variable that selects the Mode.
const EnvMode = "EGRESS_MODE"

// Mode selects which destination ranges are blocked.
type Mode string

// Values of Mode.
const (
	// ModeStrict, the default, blocks loopback, link-local and the cloud
	// metadata addresses, and the private ranges (RFC 1918, CGNAT, ULA).
	ModeStrict Mode = "strict"
	// ModeAllowPrivate blocks only loopback, link-local and the metadata
	// addresses, for clusters whose databases are on private addresses.
	ModeAllowPrivate Mode = "allow-private"
)

var (
	// ErrBlocked means the destination is in a blocked range.
	ErrBlocked = errors.New("destination is blocked by the egress policy")
	// ErrProxyUserinfo means the proxy URL carries user:password@.
	ErrProxyUserinfo = errors.New("proxy URL must not carry credentials")
	// ErrInvalidMode means EGRESS_MODE is not a Mode.
	ErrInvalidMode = errors.New("invalid egress mode")
	// ErrProxy means the proxy refused or could not set up the tunnel.
	ErrProxy = errors.New("proxy tunnel failed")
)

func prefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

var (
	// alwaysBlocked holds in both modes: unspecified, loopback, link-local
	// (which contains 169.254.169.254), multicast and the reserved top of the
	// IPv4 space, plus metadata addresses outside those ranges.
	alwaysBlocked = prefixes(
		"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fe80::/10", "ff00::/8",
		"100.100.100.200/32", "fd00:ec2::254/128",
	)
	// privateBlocked is added in ModeStrict.
	privateBlocked = prefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7")
)

// Policy is the deny list of one Mode. The zero Policy blocks nothing; tests
// use it, the operator builds its Policy with NewPolicy.
type Policy struct {
	blocked []netip.Prefix
}

// ParseMode parses EGRESS_MODE: empty is ModeStrict.
func ParseMode(v string) (Mode, error) {
	switch Mode(v) {
	case "", ModeStrict:
		return ModeStrict, nil
	case ModeAllowPrivate:
		return ModeAllowPrivate, nil
	default:
		return "", fmt.Errorf("%w %q: allowed values are %q and %q", ErrInvalidMode, v, ModeStrict, ModeAllowPrivate)
	}
}

// NewPolicy returns the Policy of a Mode.
func NewPolicy(m Mode) Policy {
	blocked := append([]netip.Prefix(nil), alwaysBlocked...)
	if m != ModeAllowPrivate {
		blocked = append(blocked, privateBlocked...)
	}
	return Policy{blocked: blocked}
}

// Check returns ErrBlocked when ip is in a blocked range. An IPv4-mapped IPv6
// address is checked as the IPv4 address it carries.
func (p Policy) Check(ip netip.Addr) error {
	ip = ip.Unmap().WithZone("")
	for _, pre := range p.blocked {
		if pre.Contains(ip) {
			return fmt.Errorf("%w: %s", ErrBlocked, ip)
		}
	}
	return nil
}

// control is net.Dialer.Control: it runs after name resolution, once per
// address the dialer is about to connect to.
func (p Policy) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrBlocked, address)
	}
	return p.Check(ap.Addr())
}

// DialFunc dials a TCP address.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Dialer returns a DialFunc that connects directly, or through proxy when it
// is not nil. Every address it connects to, the proxy's included, passes the
// Policy after resolution. With a proxy the destination is also checked when
// it is an IP address: pgconn resolves the host name itself and hands the
// address on, so a name pointing at a blocked range is stopped too.
func (p Policy) Dialer(proxy *url.URL) (DialFunc, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: p.control}
	if proxy == nil {
		return d.DialContext, nil
	}
	if proxy.User != nil {
		return nil, ErrProxyUserinfo
	}
	switch proxy.Scheme {
	case "socks5":
		// SOCKS5 fails only for a nil forward dialer, and a *net.Dialer is a
		// ContextDialer.
		sd, _ := xproxy.SOCKS5("tcp", proxy.Host, nil, d)
		cd, _ := sd.(xproxy.ContextDialer)
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			if err := p.checkTarget(addr); err != nil {
				return nil, err
			}
			return cd.DialContext(ctx, network, addr)
		}, nil
	case "http", "https":
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			if err := p.checkTarget(addr); err != nil {
				return nil, err
			}
			return connect(ctx, d, proxy, network, addr)
		}, nil
	default:
		return nil, fmt.Errorf("%w: scheme %q", ErrProxy, proxy.Scheme)
	}
}

// checkTarget checks the destination handed to a proxied dial when it is an
// IP address; a host name is left to the proxy.
func (p Policy) checkTarget(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrBlocked, addr)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return p.Check(ip)
}

// proxyTLSConfig is the TLS configuration for an https proxy; tests replace
// it to trust their proxy's certificate.
var proxyTLSConfig = func() *tls.Config { return &tls.Config{MinVersion: tls.VersionTLS12} }

// connect opens an HTTP CONNECT tunnel to addr through the proxy.
func connect(ctx context.Context, d *net.Dialer, proxy *url.URL, network, addr string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, network, proxy.Host)
	if err != nil {
		return nil, err
	}
	if proxy.Scheme == "https" {
		cfg := proxyTLSConfig()
		cfg.ServerName = proxy.Hostname()
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: %w", ErrProxy, err)
		}
		conn = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	var resp *http.Response
	err = req.Write(conn)
	if err == nil {
		resp, err = http.ReadResponse(bufio.NewReader(conn), req)
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrProxy, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: proxy answered %s", ErrProxy, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
