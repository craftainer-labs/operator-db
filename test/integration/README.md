# test/integration/

Go tests of the operator's PostgreSQL client (`internal/pgclient`) against a
real PostgreSQL 18 with TLS, behind the build tag `integration` so
`go test ./...` and the 100% coverage gate of `make test` never see them. The
unit and envtest suites use a fake behind `pgclient.Session`; this lane checks
the SQL against the engine: database create, adopt (the ownership marker) and
drop, role create, rotation and retire, grants (`ReadOnly`, `ReadWrite`,
`Owner`), extension names (including a hyphenated one), the SCRAM-SHA-256
verifier (the password never reaches the server in clear) and the termination
of a retired login's sessions, and that connections are verified (verify-full)
and refused without TLS.

- `postgres_test.go` — the tests. They read `PG_HOST` (default `localhost`),
  `PG_PORT` (`5432`), `PG_ADMIN_USER` (`postgres`), `PG_CA_FILE` (required) and
  `PG_ADMIN_PASSWORD` or `PG_ADMIN_PASSWORD_FILE` (one required). A missing
  setting fails the run; it never skips. Every test uses names of its own and
  drops what it created.
- `gen-certs.sh <dir>` — a throwaway CA, a server certificate for `postgres`,
  `localhost` and `127.0.0.1`, and a random admin password, with `openssl`.
- `pg-entrypoint.sh` — the server's entrypoint: installs the key with the
  owner PostgreSQL requires and starts the official image with `ssl=on`,
  `scram-sha-256` and a `pg_hba.conf` that accepts only `hostssl` (the local
  socket is trusted inside the container, for initdb and the health check).

- `rds_test.go` (build tags `integration rds`) — the AWS RDS backend: moto
  server answers the RDS describe calls and the PostgreSQL above is the data
  plane. Both are reached at a private container address, never loopback, with
  `allow-private` egress in the test wiring. Needs `MOTO_ENDPOINT`
  (`http://<ip>:5000`) besides the `PG_*` settings; run by the `rds` job of
  `.github/workflows/integration.yml` with `-tags integration,rds`. moto does
  not enforce IAM, so credentials and RDS IAM tokens are untested here
  ([NFR-0005](../../docs/nfrs/NFR-0005-cloud-credentials-and-egress.md)).
- `rds_iam_test.go` (same tags) — the IAM mode (NKdb-1b): the login role with
  `rds_iam` and no password, its privileges, the name-collision refusal, and the
  `iamResourceArn` built from what the probe discovers in moto. PostgreSQL is not
  RDS, so the test creates the `rds_iam` role itself; no token is validated.

- `cloudsql_test.go` (build tags `integration cloudsql`) — the Cloud SQL backend
  (NKdb-2): a fake of the Cloud SQL Admin API (sqladmin has no emulator), run as
  a Go test server on a private address of the host, answers `instances.get` with
  the PostgreSQL's address and `users.insert` and `users.delete` by creating and
  dropping the role in that PostgreSQL, as Cloud SQL does for an IAM database
  user; the PostgreSQL above is the data plane. Run by the `cloudsql` job with
  `-tags integration,cloudsql` (the `PG_*` settings only). No Google token or IAM
  login is validated ([NFR-0005](../../docs/nfrs/NFR-0005-cloud-credentials-and-egress.md)).

## Running

In CI, `.github/workflows/integration.yml` generates the certificates, starts
the pinned `postgres:18.6-alpine` with these scripts and runs the tests.

In the devcontainer, `.devcontainer/compose.instance.yml` starts the same
server as the `postgres` service (certificates from `pg-tls-init` in the
`pg-tls` volume) and sets the `PG_*` variables, so:

```sh
go test -tags integration -count=1 ./test/integration/...
```

The integration coverage of the client (informational, the gate stays on the
unit profile):

```sh
go test -tags integration -count=1 -coverpkg=./internal/pgclient/ -coverprofile=integration.cover ./test/integration/...
go tool cover -func=integration.cover
```

Anywhere else, run `gen-certs.sh` into a directory, start a server the way
`.github/workflows/integration.yml` does, and export the `PG_*` variables. The
tests dial directly: the egress policy refuses loopback and is covered by its
own unit tests.

## Don't

- `azure_test.go` (build tags `integration azure`) — the Azure backend in
  Password mode (NKdb-3a) and the Entra administrator login and IAM mode
  (NKdb-3b): a fake of Azure Resource Manager (`servers.get`) and
  the Entra authority (one fixed token, which is the password of a PostgreSQL role
  standing for the Entra administrator; `pgaadauth_create_principal` is a stub
  with the real signature, so only the SQL the operator issues is checked), run as a Go TLS test server at a private address (azcore
  and MSAL require https; the test's own certificate is trusted through
  `Config.CloudRootCAs`); the PostgreSQL above is the data plane. Run by the
  `azure` job with `-tags integration,azure` (the `PG_*` settings only). No
  Entra token is validated
  ([NFR-0005](../../docs/nfrs/NFR-0005-cloud-credentials-and-egress.md)).
- Point the tests at a server that holds anything you want to keep. They only
  create and drop databases and roles with generated names, but they connect
  as the admin role.
