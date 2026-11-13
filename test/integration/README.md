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

- Point the tests at a server that holds anything you want to keep. They only
  create and drop databases and roles with generated names, but they connect
  as the admin role.
