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
  before dropping them, so a role the claim did not create is never dropped
  whatever the caller believes: `pgclient.DropRole` refuses such a set
  (`ErrNotOwned`) and `DropOwnRoles`, which the controller uses, drops the
  roles of the claim and skips the others.
- Deleting a claim (`DEFAULT_DATA_DELETION_POLICY` and `deletionPolicy` apply
  to what the claim owns, as before):
  - A claim that never issued a create on the server (`status.externalId` is
    empty) is released without contacting the server, which may be gone. A
    claim stuck as `NameCollision` is such a claim.
  - A claim that did issue one deletes only what is its own. A database whose
    marker names another claim is skipped and the finalizer is released,
    whatever the policy. For a `DatabaseRole`, each role of the set (the owner
    role and the logins `_a` and `_b`) is checked on its own: the roles whose
    marker names the claim are dropped and the ones that are another claim's
    (or have no marker) are skipped, so a login of the claim is never left
    behind with a live credential next to a foreign role. A set with nothing of
    the claim in it drops nothing. The claim is released either way.
    A server that
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

## Install

Helm and OLM v1 need cert-manager for the webhooks' serving certificate; OLM
v0 issues its own, so it needs none. The released chart, bundle and catalog are on
GHCR, signed and attested (`release-extras.yml`, see `docs/TEMPLATE.md`).

### Helm

```sh
helm install operator-db oci://ghcr.io/craftainer-labs/charts/operator-db \
  --version X.Y.Z --namespace operator-db-system --create-namespace \
  --set egressMode=allow-private
```

A released chart pins the manager image by digest. Values worth knowing (the
schema, `charts/chart/values.schema.json`, rejects a misspelt enum or digest):

| Value | Default | Meaning |
| --- | --- | --- |
| `egressMode` | `strict` | `allow-private` is the usual choice when servers live on cluster or private addresses |
| `defaultDataDeletionPolicy` | `Retain` | the `deletionPolicy` a new `Database` is pinned to when it leaves it unset |
| `global.imageRegistry` | empty | pull the image from a mirror (air-gapped, EB-5) |
| `manager.image.digest` | set when released | image by digest; wins over `tag` |
| `manager.extraEnv` | `[]` | extra environment (not `POD_NAMESPACE`, `EGRESS_MODE` or `DEFAULT_DATA_DELETION_POLICY`: the chart sets them, and the schema rejects them here), e.g. `NO_PROXY` or `HTTPS_PROXY` for the Kubernetes API client, or `SSL_CERT_FILE` with a bundle mounted through `manager.extraVolumes` and `manager.extraVolumeMounts` |
| `manager.podSecurityContext`, `manager.securityContext` | restricted | non-root, seccomp `RuntimeDefault`, no privilege escalation, all capabilities dropped, read-only root filesystem (EB-10); the e2e installs into a `restricted` namespace as an arbitrary UID |
| `crd.keep` | `true` | keep the CRDs, and with them every claim, when the release is uninstalled |

The operator makes no outbound call except to the Kubernetes API and to the
PostgreSQL servers named by claims, and it sends no telemetry. A server's proxy
and CA bundle belong to the server object (`spec.proxyURL`, `spec.tls.caBundle`),
not to the chart.

The CSV therefore declares `features.operators.openshift.io/proxy-aware: "false"`: PostgreSQL egress is per server (`spec.proxyURL`), and the in-cluster Kubernetes API client needs no proxy. `disconnected` is `"true"` and the fips, tls-profiles and token-auth features are `"false"`.

### OLM

`make bundle` writes the bundle from `config/manifests` (`make bundle-validate`
checks it offline; `bundle/` is generated and not committed). It supports
`AllNamespaces` only, pins the manager image by digest and lists it in
`spec.relatedImages`. The released bundle and a single-bundle catalog are
`ghcr.io/craftainer-labs/operator-db-bundle` and `...-catalog`; add the catalog
as a `CatalogSource` (OLM v0) or a `ClusterCatalog` (OLM v1) and subscribe or
create a `ClusterExtension`.

## Backup and restore

The operator holds no data of its own: everything it knows is in Kubernetes
objects and in PostgreSQL.

| Kept in | What | Back it up with |
| --- | --- | --- |
| Kubernetes | the four kinds and their status, binding Secrets (`servicebinding.io/postgresql`), the admin Secrets and CA bundles that servers reference | your cluster backup (Velero, GitOps, etcd). Restore the servers and admin Secrets first, then the claims |
| The PostgreSQL server | database contents, roles' grants and passwords' verifiers, the ownership markers (`COMMENT ON DATABASE`/`ROLE`) | the server's own tooling (`pg_dump`, base backups, WAL archiving, the managed service's snapshots) |

The operator never dumps or restores data. What it does on a restore:

- Restoring claims into a cluster whose server already has the databases and
  roles: a claim whose object carries its marker (`craftainer.dev/claim=<namespace>/<name>/<uid>`)
  is re-adopted; one without it, or with a marker of another claim (a restored
  claim has a new UID), reports `NameCollision` and nothing is changed. Hand the
  object over with `COMMENT ON DATABASE "<name>" IS '<marker>'` (`ROLE` for a
  role), using the restored claim's UID.
- Restoring a server from a dump keeps the comments, so markers survive; roles
  and their passwords come back with a `pg_dumpall --roles-only`. A restored
  login has the password of the dump, which no longer matches the binding
  Secret: rotate the `DatabaseRole` to issue a new one.
- Binding Secrets are rewritten by the operator, so a restore of them is not
  required.

`deletionPolicy` (on `Database` only; `DatabaseRole` always drops its roles)
decides what deleting a claim does to the data:

- `Retain` (the default; `DEFAULT_DATA_DELETION_POLICY` sets it for a claim that
  leaves it unset, pinned in `status.effectiveDeletionPolicy`): the database
  and its data stay on the server, the finalizer is released. A later claim of
  the same name needs the marker handed over (above).
- `Delete`: the database is dropped with `DROP DATABASE ... WITH (FORCE)`.
  Take a backup first; there is no undo.
- Either way, an object whose marker names another claim is skipped and the
  finalizer released, and a claim that never issued a create touches nothing
  (see "Deleting a claim").
- Uninstalling the chart with `crd.keep=false`, or deleting a CRD, deletes every
  claim of that kind and applies each claim's policy.

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
