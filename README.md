# operator-db

> [!NOTE]
> This repository is vibe coded with [Claude](https://claude.com/product/claude-code).

A Kubernetes operator that provisions PostgreSQL databases and roles inside an
existing server, from Kubernetes objects. It is an instance of
[`template-k8s-operator`](https://github.com/craftainer/template-k8s-operator)
(and so of `template-docker` and `template-base`); `docs/TEMPLATE.md` describes
the layout, tooling and release flow it inherits, and is kept up to date by
template sync.

## API group `db.craftainer.dev`

All kinds are `v1alpha1`. The fields, defaults and validation follow
[`docs/crd-drafts.md`](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/crd-drafts.md)
and [ADR 0002](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md)
of `lib-operator-go`, whose `pkg/apis/common` types the API embeds.

| Kind | Scope | Purpose |
| --- | --- | --- |
| `ClusterDatabaseServer` | cluster | a PostgreSQL server; `allowedNamespaces` says which namespaces may use it |
| `DatabaseServer` | namespaced | a server a team brings itself; serves only its own namespace |
| `Database` | namespaced | a database on a server (data kind: `deletionPolicy`, no binding Secret) |
| `DatabaseRole` | namespaced | a login with `ReadOnly`, `ReadWrite` or `Owner` access to one `Database`; credentials go to a `servicebinding.io/postgresql` Secret |

The CRDs are generated from the Go types in `api/v1alpha1` (`make manifests
generate`) into `config/crd/bases` and `charts/chart/templates/crd`. Schema
defaults and the OpenAPI and CEL rules are in the CRD; the validating webhooks in
`internal/webhook/v1alpha1` check only what the schema cannot (a server's `host`
and `proxyURL`).

## Controllers

`Database` and `DatabaseRole` are reconciled by `internal/controller` over
`lifecycle.Reconcile` of `lib-operator-go` (finalizer, `deletionPolicy`,
`managementPolicies`, drift polling, two-phase rotation, conditions and Events).
The PostgreSQL client is `internal/pgclient` (`jackc/pgx/v5`): identifiers are
always quoted with `pgx.Identifier`, and extension names must match
`^[a-z_][a-z0-9_-]*$` (in the CRD and again in the controller).

- A `Database` is created from `template0`, named from its namespace and name
  with a hash, and stamped with an ownership marker in `COMMENT ON DATABASE`;
  `spec.extensions` must be in the server's `allowedExtensions`, and an
  extension removed from the spec is never dropped.
- A `DatabaseRole` is a `NOLOGIN` owner role with two `LOGIN` roles, `_a` and
  `_b`, that `SET ROLE` to it. A rotation sets a new password on the other
  login and retires the previous one after `rotationPolicy.gracePeriod`.
  `privileges` become grants on the `public` schema of the database; objects an
  `Owner` role creates after a `ReadOnly` or `ReadWrite` role was granted are
  covered at that role's next privilege change. An update of the role (a changed
  `privileges` or `connectionLimit`, or drift) issues the active login a new
  password and rewrites the binding Secret.
- The operator only manages what it can show it created. Every role is created
  together with its ownership marker, and `Database` records in
  `status.externalId` that it issued the create before `CREATE DATABASE` runs
  (that statement cannot share a transaction with `COMMENT ON DATABASE`), and
  `DatabaseRole` does the same before its first statement. A
  database or role that exists without a marker of the claim is reported as a
  `NameCollision` condition ("database exists and is not managed by this
  operator" or "belongs to another claim") and is never changed or dropped.
  Deleting a claim in that state leaves the object alone and releases the
  finalizer (see "Deleting a claim" below). To resolve it, either rename the
  claim, or `COMMENT ON DATABASE "<name>" IS 'craftainer.dev/claim=<namespace>/<name>/<uid>'`
  (`ROLE` for a role) to hand the object over.
- Login passwords never reach the server in clear: the operator computes the
  SCRAM-SHA-256 verifier (RFC 5802; passwords are ASCII, for which SASLprep is
  the identity) and sends `ALTER ROLE ... PASSWORD 'SCRAM-SHA-256$...'`, which
  PostgreSQL stores as it is, so statement logging and `pg_stat_statements`
  hold only the verifier ([NFR-0004](docs/nfrs/NFR-0004-least-privilege-and-no-credentials-in-output.md)).
  Retiring a login (`NOLOGIN`, password cleared) also ends its open sessions
  with `pg_terminate_backend`, which needs the admin role to be a superuser or
  a member of `pg_signal_backend`. Without it the retire still succeeds,
  because `NOLOGIN` and the cleared password already refuse every new session:
  the operator logs a warning, emits a `SessionsNotTerminated` Warning Event
  and sets the `SessionsTerminated` condition to `False` with that reason
  (cleared by the next retire that ends the sessions). Any other error on that
  statement fails the retire. Deleting a `DatabaseRole` likewise drops its roles
  without ending their sessions when the privilege is missing.
- The admin connections of a server are reused between reconciles: one
  `pgxpool` per server (at most two connections, idle ones closed after 30
  seconds, health-checked every 30 seconds, a broken one discarded when it is
  released), only for the admin database; a claim's database is opened and
  closed per use, because `DROP DATABASE ... WITH (FORCE)` would kill a pooled
  session. The pool is keyed on the server's UID and generation and a hash of
  the resolved connection (host, port, credentials, CA, TLS settings, proxy), so
  a rotated admin Secret or CA replaces it (the old pool is closed in the
  background; a reconcile that still holds it fails once as an unreachable
  server and is retried), and it is dropped when the server is gone or
  invalid, or unused for ten minutes.
- A `DatabaseRole` is reconciled again when the `Database` it names changes (a
  watch through a field index), so a role waiting for its `Database` goes on as
  soon as the `Database` has its name.
- Deleting a `DatabaseRole` checks the ownership marker of its roles again
  before dropping them, so a role the claim did not create is refused
  (`ErrNotOwned`) whatever the caller believes.
- Deleting a claim (`DEFAULT_DATA_DELETION_POLICY` and `deletionPolicy` apply
  to what the claim owns, as before):
  - A claim that never issued a create on the server (`status.externalId` is
    empty) is released without contacting the server, which may be gone. A
    claim stuck as `NameCollision` is such a claim.
  - A claim that did issue one deletes only what is its own. A database whose
    marker names another claim is skipped and the finalizer is released,
    whatever the policy. For a `DatabaseRole`, a role of the set (the owner
    role and the logins `_a` and `_b`) that is not the claim's makes the
    whole set foreign: none of it is dropped and the claim is released, because
    dropping the rest would need to know which part is ours. A server that
    cannot be reached still keeps the finalizer for a claim that did issue a
    create.
  - "Issued a create" is `status.externalId`: `create-issued`, written before
    `CREATE DATABASE` or the first role statement (withdrawn when the create
    finds the object is somebody else's). A claim from before the record
    existed has none, and deleting it releases the claim and leaves the object,
    the safe reading when the status does not say it is ours. A reconcile gives
    such a claim the record when the object carries its marker, so only a claim
    deleted before its next reconcile is affected.
- Known limits: a session the server closed costs one failed reconcile before
  the pool opens a new connection; objects an
  `Owner` role creates later are covered only at the next privilege change (see
  above).
- Server health probing (`ClusterDatabaseServer` and `DatabaseServer` status) is
  not in yet.

Configuration, as flags with environment defaults:

| Variable | Flag | Meaning |
| --- | --- | --- |
| `DEFAULT_DATA_DELETION_POLICY` | `--default-data-deletion-policy` | `Retain` (default) or `Delete`, pinned into a new `Database` that leaves `deletionPolicy` unset; anything else fails startup |
| `EGRESS_MODE` | `--egress-mode` | `strict` (default) blocks loopback, link-local and metadata, and private ranges; `allow-private` blocks all but the private ranges ([NFR-0003](docs/nfrs/NFR-0003-restricted-egress-and-proxy.md)) |
| `POD_NAMESPACE` | `--operator-namespace` | where a cluster server's admin Secret is read; empty uses the service account's namespace, and startup fails when neither names one |

The chart sets the first two from the values `egressMode` (default `strict`)
and `defaultDataDeletionPolicy` (default `Retain`).

The tests of the controller run against the Kubernetes API (envtest) with a fake
PostgreSQL behind `pgclient.Session`. The SQL runs against a real PostgreSQL 18
with TLS (verify-full, scram-sha-256) in the integration lane,
[`test/integration`](test/integration/README.md) (build tag `integration`;
`.github/workflows/integration.yml` in CI, the `postgres` service of the
devcontainer locally); the 100% coverage gate stays on the unit profile. The non-functional requirements are in
[`docs/nfrs`](docs/nfrs).

## Quick start

Open the repository in its devcontainer (the Go and Kubernetes tools are
installed there), then:

```sh
make test          # unit + envtest, race detector, coverage gate
make test-e2e      # kind cluster + Chainsaw (needs ~2 GB free memory and Docker)
make helm-lint     # lint the chart, check it matches config/
make bundle-validate   # OLM bundle (operator-sdk) and catalog (opm)
make operator-help # every operator target
prek run --all-files --hook-stage manual   # all checks
```

Releases go through `release.yml`; right after each one, dispatch
`release-extras.yml` to publish the chart, bundle and catalog (see
`docs/TEMPLATE.md`):

```sh
gh workflow run release-extras.yml -f tag=vX.Y.Z
```

## License

Licensed under the [GNU General Public License v3.0](LICENSE).
