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

The controllers, the PostgreSQL client and the chart's operator settings are not
in yet; the manager serves the webhooks and the probes.

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
