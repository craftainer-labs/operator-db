# 0005. Cloud backends

## Status

Proposed. The AWS section is implemented by NKdb-1a (password mode) and
NKdb-1b (IAM mode); GCP (NKdb-2) and Azure (NKdb-3) follow and are added to this
record as they merge. It becomes Accepted when the last of them merges.

## Context

A `ClusterDatabaseServer` or `DatabaseServer` today names a PostgreSQL host.
The plan's Track NK adds managed cloud databases behind the same claim kinds,
naming, lifecycle and egress rules. The shapes are decided in
[ADR 0002 section 16 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md)
and `docs/crd-drafts.md`, "Cloud providers"; `pkg/cloud` (v0.5.0) resolves the
credentials and builds the AWS client. The library's `common.ServerStatus` has
no `discovered` field, and its probe reports only conditions it chooses.

## Decision

### AWS (RDS and Aurora PostgreSQL)

- **API.** `spec.provider` (`type: AWS`, `aws: {region, roleARN?,
  endpointOverride?, dbInstanceIdentifier | dbClusterIdentifier}`) and
  `spec.credentials` (`source: WorkloadIdentity | Secret`) on both server kinds.
  A namespaced server has no `endpointOverride` and must use `Secret` (types
  `cloud.AWSBlock` and `cloud.NamespacedCredentials`, plus CEL). CEL on the
  server spec: `host` is required only without a provider (with one it
  overrides the discovered address, for an emulator); `credentials` only with
  a provider; the provider union; exactly one target identifier; only `AWS` is
  accepted until the other clouds land. `provider.type` is immutable.
  `adminCredentialsSecretRef` stays required: the operator logs in to the data
  plane as an administrator with a password. IAM administrator login is Azure
  only (decision 4).
- **Control plane.** The probe resolves the instance (`DescribeDBInstances`) or
  the Aurora cluster writer endpoint (`DescribeDBClusters`) with the
  credentials of `spec.credentials`, and records `endpoint`, `port`,
  `resourceId`, `accountId` and `region` in `status.discovered` (the type
  `DatabaseServerStatus` embeds `common.ServerStatus` and adds the map; the
  probe reconciler writes status with a merge patch, which carries it).
  Claims read the cached server's `status.discovered`; a server that has not
  been resolved fails with `ErrNotDiscovered` and is retried. A server's
  credentials Secret is read by the controller; on a cluster server it must be
  in the operator's namespace (`CredentialsNamespaceForbidden`), as the admin
  Secret must be.
- **Address precedence.** `spec.host` (with `spec.port`, default 5432) wins
  when set; otherwise the discovered endpoint and port are used. A transient
  cloud failure (throttling, a timeout) keeps the last discovered address; a
  missing target or refused credentials drop it. A discovered endpoint must be
  a plain DNS name, since the admin password is sent to it.
- **Data plane.** The existing pgconn path against the discovered (or
  overridden) address, with the same verified TLS. Password mode reuses the
  role logic of the self-hosted backend; the binding gains `cloud`,
  `authMode: Password` and `region`.
- **IAM mode (AWS)** is `authMode: IAM` on a `DatabaseRole` of an AWS server
  (see "AWS IAM mode" below). On a server that is not a cloud one the claim is
  still refused with `AuthModeUnsupported` and creates nothing, as it is
  for the other clouds until NKdb-2 and NKdb-3. `authMode` is immutable, and CEL
  forbids `rotationPolicy` with `IAM`.
- **Reasons.** The library has no constants for the new reasons; they are local
  (`internal/controller/cloud.go`): `CredentialsNamespaceForbidden`,
  `CredentialsUnavailable`, `TargetNotFound`, `EndpointOverrideForbidden`,
  `CloudError` (server) and `AuthModeUnsupported` and `RotationNotApplicable`
  (claim). The probe
  reconciler fixes the reason of `Ready` and `ServerReachable`, so the server
  reasons are on a separate condition, `CloudResolved`, and a claim's
  `AuthModeUnsupported` is a Warning Event with that reason (the lifecycle sets
  the claim's condition reason to `ReconcileError`). The probe persists a
  status change only when it changes a condition or on its refresh interval,
  so a reason that changes while `Ready` stays False can take up to the
  refresh interval (10 minutes) to show.
- **Egress and errors.** Every AWS HTTP client comes from `cloud.Egress`,
  whose dialer is the egress policy, STS included. Errors are categories:
  the SDK error text, ARNs, keys and the Secret contents never reach a
  condition, Event, status or log from this code.
- **IAM permissions the operator needs (AWS):** `rds:DescribeDBInstances` and
  `rds:DescribeDBClusters` on the target. The user attaches the role to the
  operator's ServiceAccount; the operator creates no identity.

### AWS IAM mode

- **Role.** One login role, named like the owner role of password mode (the
  `naming.Tenant` name of the claim, no `_a` or `_b`), created with
  `CREATE ROLE <name> LOGIN` and no password, then `GRANT rds_iam TO <name>`. It
  gets the privileges of the claim directly (database `CONNECT`, and the schema,
  table and sequence grants and default privileges of `ReadOnly`, `ReadWrite`
  or `Owner`, as in password mode) and the claim's connection limit. Every
  identifier is quoted with `pgx.Identifier` and every literal with `Literal`.
  `rds_iam` is RDS's own role; the operator grants it and never creates it. A
  stored password is cleared (`PASSWORD NULL`). Unlike password mode, which
  logs in as a member of a NOLOGIN owner role (`SET role`), the privileges are
  the login's own, as ADR 0002 section 16.4 specifies, so objects an IAM login
  creates are owned by that login.
- **Ownership.** The role is created with the claim marker in one query
  (`COMMENT ON ROLE`, ADR 0002 section 10), and the full marker (with the
  privileges) is written after the grants. The marker is checked before any
  statement: a role of that name that exists without this claim's marker, or
  with another claim's, is never adopted, changed or dropped (`NameCollision`
  from Observe; `ErrNotOwned` from the apply, if it appeared in between). The
  check runs in PostgreSQL on the role.
- **Binding Secret.** `type`, `provider`, `cloud` (`aws`), `authMode` (`IAM`),
  `region`, `host`, `port`, `database`, `username` (the role), `sslmode`,
  `sslrootcert` and `iamResourceArn`
  (`arn:aws:rds-db:<region>:<account>:dbuser:<resourceId>/<role>`, from
  `status.discovered`: the partition is always `aws`, since v1alpha1 has no
  sovereign clouds). There is **no `password` and no `uri`**, which would carry
  one. A server whose account, resource id or region has not been discovered
  yet fails the claim (`ErrIAMTargetUnknown`) before anything is created. The
  consumer's workload role needs `rds-db:connect` on that ARN and builds a
  token (15 minutes) for `username` with the AWS SDK, as the password; the
  operator builds no token.
- **No rotation.** The IAM external is not a `lifecycle.Rotator`, so
  `status.rotation` stays empty. The `craftainer.dev/rotate` annotation gives a
  Warning Event with the reason `RotationNotApplicable`.
- **Deletion** drops the role as password mode does (sessions ended, owned
  objects reassigned to the admin role), only when the marker is the claim's.
- **Admin connection** is unchanged: the AWS server's administrator is the
  password Secret (decision 4); IAM administrator login is Azure only.
- **Known limits of the library (lib-operator-go v0.5.0).** Because the IAM
  external is not a Rotator, (1) the library reports the rotate annotation also
  as `Synced=False` with its `RotationUnsupported` reason, next to our Event, and
  (2) a binding Secret that is deleted is not recreated: the claim reports
  `BindingSecretMissing`, because only the rotate path may issue a credential
  again and there is none to issue. The Secret holds nothing secret in this
  mode; recreating it by a plain write needs a library change (an `External`
  that says its binding can be rebuilt from `Update`), which is the follow-up
  to raise for a lib minor release.

## Consequences

- The drafts' `discovered` lives in a local status type until the library's
  `ServerStatus` has it; moving it later is not a CRD change.
- Cloud-server tests run against moto (API shape only; moto does not enforce
  IAM) and a TLS PostgreSQL container as the data plane, at a private address
  with `EGRESS_MODE=allow-private` in the test wiring only.
  [NFR-0005](../nfrs/NFR-0005-cloud-credentials-and-egress.md) records what
  CI cannot verify. The IAM role's SQL runs against PostgreSQL 18 with a
  stand-in `rds_iam` role; no emulator validates an IAM token, so the login with
  a token is untested in CI.
- The operator now imports the AWS SDK (`service/rds`), which grows its module
  graph; all modules are Apache-2.0.
