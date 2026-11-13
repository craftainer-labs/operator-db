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
  proxy host is checked the same way; behind a proxy the destination is checked
  when it is an address (pgconn resolves the name before it dials).
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
- The proxy may be `http`, `https` (CONNECT, TLS 1.2 or newer) or `socks5`.

## Source

Plan Decision log, 2026-11-03 (ADR 0002 open items); plan rows EB-3 and EB-5;
[ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), "Open issues".

## Verification

`TestPolicyCheck`, `TestControl`, `TestDirectDialIsCheckedAfterResolution`,
`TestHTTPProxyTunnel`, `TestHTTPSProxyTunnel`, `TestSOCKS5ChecksTheDestination`
and `TestProxyUserinfoAndScheme` (`internal/egress`) cover the range tables of
both modes and the dialers; `TestValidateServer` (`internal/webhook`) and
`TestDatabaseOnAnInvalidServer` (`internal/controller`) cover the userinfo
rejection; `TestControllerSettings` and `TestRunRejectsInvalidSettings`
(`internal/app`) cover the environment variable.
