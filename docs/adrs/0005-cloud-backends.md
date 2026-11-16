# 0005. Cloud backends

## Status

Proposed. The AWS section is implemented by NKdb-1a (password mode); IAM mode
(NKdb-1b), GCP (NKdb-2) and Azure (NKdb-3) follow and are added to this record
as they merge. It becomes Accepted when the last of them merges.

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
- **IAM mode** is not implemented yet: a `DatabaseRole` with `authMode: IAM`
  is refused with `AuthModeUnsupported` and creates nothing. `authMode` is
  immutable, and CEL forbids `rotationPolicy` with `IAM`. NKdb-1b adds the
  role (`CREATE ROLE ... LOGIN; GRANT rds_iam`), its ownership marker and
  `NameCollision`, `iamResourceArn` in the binding and `RotationNotApplicable`.
- **Reasons.** The library has no constants for the new reasons; they are local
  (`internal/controller/cloud.go`): `CredentialsNamespaceForbidden`,
  `CredentialsUnavailable`, `TargetNotFound`, `EndpointOverrideForbidden`,
  `CloudError` (server) and `AuthModeUnsupported` (claim). The probe
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

## Consequences

- The drafts' `discovered` lives in a local status type until the library's
  `ServerStatus` has it; moving it later is not a CRD change.
- Cloud-server tests run against moto (API shape only; moto does not enforce
  IAM) and a TLS PostgreSQL container as the data plane, at a private address
  with `EGRESS_MODE=allow-private` in the test wiring only.
  [NFR-0005](../nfrs/NFR-0005-cloud-credentials-and-egress.md) records what
  CI cannot verify.
- The operator now imports the AWS SDK (`service/rds`), which grows its module
  graph; all modules are Apache-2.0.
