# NFR-0002. Every PostgreSQL connection is TLS 1.2 or newer and verifies the server

## Status

Implemented

## Attribute

Security (encryption in transit, FIPS-capable build, plan row EB-14).

## Description

- The connection is TLS only: there is no plaintext fallback, and
  `sslMode` offers no `disable`, `allow` or `prefer`.
- The server certificate chain and host name are verified against the
  server's CA bundle (`tls.caBundle`, key `ca.crt` unless the reference names
  another; the system roots when there is none), with `tls.serverName`
  overriding the name. `InsecureSkipVerify` is never set. `verify-ca` and
  `require` are therefore verified as `verify-full` (stricter, [ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md),
  section 13); a server whose certificate names another host sets
  `tls.serverName`.
- The TLS floor is `tls.minVersion` (1.2 by default, 1.3 on request).
- Passwords are stored as SCRAM-SHA-256 (`password_encryption` is set on every
  session), never MD5.

## Source

Plan row EB-14; [ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), section 13.

## Verification

`TestConnectVerifiesTheServer`, `TestConnectAndSession` and
`TestTLSConfig` (`internal/pgclient`) connect to a TLS server with a test CA
and fail for an untrusted CA, another CA and a wrong name;
`TestTargetFollowsTheServer` (`internal/controller`) checks that the
server's bundle, name and floor reach the connection. The `fips140=only` run
is the library's (lib-operator-go); this repository's release build follows
`docs/TEMPLATE.md`.
