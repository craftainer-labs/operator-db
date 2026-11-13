# NFR-0003. A tenant cannot aim the operator at internal addresses

## Status

Implemented

## Attribute

Security (server-side request forgery, multi-tenancy, plan rows EB-3 and EB-5).

## Description

A namespaced DatabaseServer lets a tenant name any host. Decided by the user on
2026-11-03:

- Every connection is checked after the host name has been resolved, when the
  socket is about to connect (`net.Dialer.Control`), so a name that resolves
  into a blocked range is stopped as well as a literal address. The server's
  proxy host is checked the same way. Behind a proxy the destination must be an
  IP address, which pgconn provides because it resolves the name before it
  dials; a host name is refused, because the proxy would resolve it where this
  policy cannot see the answer.
- An IPv6 address that carries an IPv4 address is checked as that IPv4 address:
  IPv4-mapped (`::ffff:`), NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`), 6to4
  (`2002::/16`) and Teredo (`2001::/32`). The always-blocked set also holds
  192.0.0.0/24, 198.18.0.0/15 and 168.63.129.16.
- The environment variable `EGRESS_MODE` (flag `--egress-mode`) selects what is
  blocked. `strict`, the default, blocks loopback, link-local (which holds
  the cloud metadata address 169.254.169.254), the other metadata addresses,
  multicast and reserved ranges, and the private ranges (RFC 1918, CGNAT
  100.64.0.0/10, ULA fc00::/7). `allow-private` blocks all but the private
  ranges, for clusters whose databases are on private addresses. Any other value
  fails startup.
- `proxyURL` may not carry `user:password@`: the webhooks reject it, and the
  controller refuses to dial through such a URL even when admission was
  bypassed. Credentials come only from Secret references. Proxy URLs and their
  userinfo are never echoed in errors, conditions or Events.
- The proxy may be `http`, `https` (CONNECT, TLS 1.2 or newer) or `socks5`. A
  proxy URL without a port uses the scheme's default: 80, 443 and 1080.
- A failed connection reaches conditions and Events as one fixed text per
  category (refused by the egress policy, TLS handshake failed, authentication
  failed, timed out, connection failed), never an address or the raw error, so a
  tenant gets no port scan or name resolution out of the operator. The raw error
  is in the operator log.

## Source

Plan Decision log, 2026-11-03 (ADR 0002 open items); plan rows EB-3 and EB-5;
[ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), "Open issues".

## Verification

`TestPolicyCheck`, `TestControl`, `TestDirectDialIsCheckedAfterResolution`,
`TestHTTPProxyTunnel`, `TestHTTPSProxyTunnel`, `TestSOCKS5ChecksTheDestination`
`TestCheckTarget` and `TestProxyAddrDefaultPorts` (`internal/egress`),
`TestConnectionErrorsAreCoarse` (`internal/pgclient`),
`TestUnreachableServerShowsOnlyACategory` (`internal/controller`)
and `TestProxyUserinfoAndScheme` (`internal/egress`) cover the range tables of
both modes and the dialers; `TestValidateServer` (`internal/webhook`) and
`TestDatabaseOnAnInvalidServer` (`internal/controller`) cover the userinfo
rejection; `TestControllerSettings` and `TestRunRejectsInvalidSettings`
(`internal/app`) cover the environment variable.
