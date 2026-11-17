# 0005. Cloud backends

## Status

Proposed. The AWS section is implemented by NKdb-1a (password mode) and
NKdb-1b (IAM mode), the GCP section by NKdb-2, and the Azure section (Password
mode) by NKdb-3a. NKdb-3b (the Entra administrator login and IAM mode) waits for
lib-operator-go v0.6.0 and is added to this record as it merges. It becomes
Accepted when the last of them merges.

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
  accepted until the other clouds land (AWS and GCP now). `provider.type` is
  immutable.
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
- **IAM mode** is `authMode: IAM` on a `DatabaseRole` of an AWS or a GCP server
  (see "AWS IAM mode" and "GCP" below). On a server that is not one of these
  (self-hosted, or a cloud whose IAM mode is not implemented yet, Azure until
  NKdb-3) the claim is refused with `AuthModeUnsupported` and creates nothing:
  the gate names the providers that support IAM, it is not "any provider".
  `authMode` is immutable, and CEL forbids `rotationPolicy` with `IAM`.
- **Reasons.** The library has no constants for the new reasons; they are local
  (`internal/controller/cloud.go`): `CredentialsNamespaceForbidden`,
  `CredentialsUnavailable`, `TargetNotFound`, `EndpointOverrideForbidden`,
  `CloudError` (server) and `AuthModeUnsupported`, `RotationNotApplicable` and
  `IAMPrincipalRequired` (claim). The probe
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
  creates are owned by that login, and deleting the claim reassigns them to the
  admin role (`REASSIGN OWNED`), as in password mode.
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

### GCP (Cloud SQL for PostgreSQL)

- **API.** `spec.provider` takes `type: GCP` and `gcp: {project, location,
  instance, endpointOverride?}` (`cloud.GCPBlock` / `cloud.GCPClusterBlock`
  plus `instance`, pattern `^[a-z]([-a-z0-9]*[a-z0-9])?$`, at most 98). A
  Cloud SQL instance is regional, so CEL requires `location` on both kinds
  (the drafts say "where regional"). CEL on the server spec now admits `AWS`
  and `GCP`, and each block is required exactly with its type. The rules of
  AWS apply unchanged: `host` is optional with a provider, a namespaced server
  must use `credentials.source: Secret`, `endpointOverride` is on cluster
  kinds only and honoured only in `allow-private`. `DatabaseRole` gains
  `spec.iam.principal` (1 to 320 characters, immutable; `iam` is allowed only
  with `authMode: IAM`), the field of the drafts' `CloudAuth`.
- **Credentials.** `credentials.source` is `WorkloadIdentity` (Application
  Default Credentials) or `Secret` with the key `credentials.json`, a
  `service_account` key only (decision 10); `cloud.Egress.GCP` resolves it and
  its HTTP client dials through the egress policy, the token endpoint
  included. Errors are categories: the SDK text, the key, the project and the
  principal never reach a condition, Event or log from this code.
- **Discovery.** The probe calls `instances.get` of the Cloud SQL Admin API
  (`sqladmin/v1`) with that client and records `endpoint`, `port` (5432, the
  port of every Cloud SQL PostgreSQL instance), `region`,
  `instanceConnectionName` and `dnsName` in `status.discovered`. The
  connection name the API reports must equal `project:location:instance`,
  derived from the spec: the API looks an instance up by project and name only,
  so this is what checks `location` (otherwise `TargetNotFound`). An instance
  that is not PostgreSQL is refused. The endpoint is the instance's IP address:
  the private one when the operator runs with `EGRESS_MODE=allow-private` and
  the instance has one, else the public one; an address that is not routable
  unicast (loopback, link-local, unspecified, multicast) is ignored. Whatever is
  chosen, the egress policy still decides at dial time, so a private IP needs
  `allow-private`. `dnsName` (reported with a trailing dot, which is removed)
  is kept only when it is a plain DNS name.
- **Admin connection.** The password Secret of `adminCredentialsSecretRef`
  (decision 4), over the same verified TLS as every other server, to the
  discovered IP address, **not** through `cloudsqlconn`. Reasons: the connector
  needs the Cloud SQL Admin API for the ephemeral certificate of every
  connection and dials its own sockets, which would need a bridge to the
  operator's egress dialer; the admin login is a password, so the connector's
  IAM authentication would not be used; and the connector cannot be tested
  against the CI data plane (a PostgreSQL container). The server certificate of
  an instance is checked against `tls.caBundle` (the instance's server CA) and,
  as it names a host and not an IP address, the TLS server name defaults to the
  discovered `dnsName` when `tls.serverName` is unset and `spec.host` is not
  used; an explicit `tls.serverName` wins. An instance whose certificate carries
  no DNS name (an older server CA mode) needs `tls.serverName` set to a name the
  certificate does carry, which Go can only match as a DNS or IP SAN: such an
  instance is not reachable by this path, and the operator does not skip the
  verification. `cloudsqlconn` stays a module dependency pinned by
  `internal/clouddeps`, because it is the consumer's connector (below).
- **Password mode** reuses the role logic of the self-hosted backend. The
  binding gains `cloud` (`gcp`), `authMode: Password` and
  `instanceConnectionName`, and no `region` (that key is AWS only).
- **IAM mode.** `iam.principal` is required (`IAMPrincipalRequired` otherwise,
  also for a malformed one, and refused on AWS) and names the database user:
  the e-mail of a Google account, or the e-mail of a service account without
  `.gserviceaccount.com` (`name@project.iam`), in lower case and at most 63
  characters, because it is also a PostgreSQL role name. The user type is read
  from the form: a name ending in `.iam` is `CLOUD_IAM_SERVICE_ACCOUNT`, any
  other is `CLOUD_IAM_USER`. The operator
  - checks the role in PostgreSQL first, as ADR 0002 section 16.4 requires: a
    role of that name that exists without the claim's marker, or with another
    claim's, is a `NameCollision` and nothing is called;
  - otherwise calls `users.insert` (the Admin API answers with a long-running
    operation, which the operator follows with `operations.get` for up to two
    minutes, so the marker is written in the same call), and sets the marker
    (`COMMENT ON ROLE`) right after it; a crash between the two leaves a role
    without a marker that reads as a `NameCollision` until an administrator
    removes it: it fails closed;
  - applies the connection limit (only when it differs) and the same grants as
    password mode directly to that role, then writes the full marker last.
  Cloud SQL forces the principal-derived role name, the one exception to "a
  claim cannot choose its external name" (decision 6, ADR 0002 16.4); the claim
  names it in `iam.principal`, which is immutable, and the role name is kept in
  `status.externalName`, from which the delete is done. A tenant can name any
  principal in the project and give it access to its own database; it gains
  nothing outside that database. The IAM user has no password and is not a
  member of any role the operator creates.
- **Binding Secret (IAM).** `type`, `provider`, `cloud` (`gcp`), `authMode`
  (`IAM`), `instanceConnectionName`, `host`, `port`, `database`, `username`
  (the principal), `sslmode` and `sslrootcert`. There is **no `password`, no
  `uri`, and no `region` or `iamResourceArn`**. The consumer connects through
  the Cloud SQL connector with IAM authentication (`cloudsqlconn`
  `WithIAMAuthN`), which refreshes the token itself; `host` and `port` are
  there for a consumer that goes through the instance's address. No rotation
  (`RotationNotApplicable` Event on the annotation, `status.rotation` empty).
- **Deletion.** Only a role that carries the claim's marker is touched. Its
  sessions are ended (best effort, as in password mode), the admin role is made
  a member of it (best effort; `REASSIGN OWNED` needs it) and takes over the
  objects it owns (`REASSIGN OWNED`, `DROP OWNED`), and then `users.delete`
  removes the user, which drops the role. A user that is gone already is not an
  error. A failing `users.delete` keeps the claim and its finalizer.
- **IAM permissions the operator needs (GCP):** `cloudsql.instances.get`,
  `cloudsql.users.create` and `cloudsql.users.delete` on the instance (for
  example the predefined `roles/cloudsql.admin`, or a custom role), and
  `cloudsql.instances.get` alone for a server that serves only Password
  claims. The operator creates no identity or grant.

### Azure (Database for PostgreSQL flexible server), Password mode

- **API.** `spec.provider` takes `type: Azure` and `azure: {tenantID,
  subscriptionID, resourceGroup, serverName, endpointOverride?}`
  (`cloud.AzureBlock` / `cloud.AzureClusterBlock` plus `serverName`, 3 to 63
  lowercase letters, digits and inner hyphens). The library leaves
  `subscriptionID` and `resourceGroup` optional because Graph-based operators
  have none; a flexible server cannot be addressed without them, so CEL
  requires both on both kinds. CEL on the server spec now admits `AWS`, `GCP`
  and `Azure`, and each block is required exactly with its type. The rules of
  AWS apply unchanged: `host` is optional with a provider, a namespaced server
  must use `credentials.source: Secret` (keys `clientId` and `clientSecret`),
  `endpointOverride` is on cluster kinds only and honoured only in
  `allow-private`. `adminAuthMode` and `provider.azure.entraAdmin` (the Entra
  administrator login, decision 4 of the G-api) are not part of this change:
  the library's `ServerCommon` still requires `adminCredentialsSecretRef` and
  `server.Resolve` always reads it, so an administrator without a Secret needs
  the cloud-server types of lib-operator-go v0.6.0 (NKdb-3b). The field name
  there is `entraAdmin`.
- **Credentials.** `credentials.source` is `WorkloadIdentity` (the Azure
  workload identity webhook's variables) or `Secret`; `cloud.Egress.Azure`
  resolves it and its HTTP client dials through the egress policy, the Entra
  authority included. With `endpointOverride` the override is both the Entra
  authority (which MSAL requires to be https) and the Resource Manager
  endpoint, which is what an emulator that serves both needs. Errors are
  categories: the SDK text, the subscription, the resource group and the
  server name never reach a condition, Event or log from this code. A
  credential the authority refuses is `CredentialsUnavailable`; an authority
  that cannot be reached is transient. `endpointOverride` is for emulators: the
  Entra token minted for the public Resource Manager audience is sent to the
  override host, which only a cluster administrator in `allow-private` mode can
  set. `resourceGroup` must not end with a period, which also rules out `.` and
  `..` in the request path.
- **Discovery.** The probe calls `servers.get` of Resource Manager
  (`armpostgresqlflexibleservers/v6`, resource provider registration off: the
  operator only reads) and records `endpoint` (the server's
  `fullyQualifiedDomainName`), `port` (5432), `region` (the server's location)
  and `resourceId` (the ARM id) in `status.discovered`. The host name must be a
  plain DNS name and the server's state `Ready` (a stopped or starting server
  has no usable address for now: a `CloudError`, transient only while it is
  Starting or Updating). A missing server is `TargetNotFound`;
  throttling, timeouts and 5xx answers are transient (the last address is kept).
- **Admin connection and roles.** The password Secret of
  `adminCredentialsSecretRef`, over the same verified TLS as every other server,
  to the server's host name. The server's certificate chains to a public root
  that is not always in the operator's image, so set `tls.caBundle` when the
  system roots are not enough. Tenant roles are today's Password-mode logic
  unchanged (the admin of a flexible server is a member of `azure_pg_admin`,
  which may create roles); the binding carries `cloud: azure` and
  `authMode: Password`. `authMode: IAM` is refused with `AuthModeUnsupported` on
  Azure until NKdb-3b.
- **Permissions.** The operator's identity needs `Microsoft.DBforPostgreSQL/
  flexibleServers/read` on the server (for example the built-in Reader role
  scoped to it). The operator creates no identity or grant.

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
- The operator now imports the AWS SDK (`service/rds`), the Cloud SQL Admin
  API client (`google.golang.org/api/sqladmin/v1`) and the Azure PostgreSQL
  flexible servers management client (`armpostgresqlflexibleservers/v6`), which
  grow its module graph; all modules are Apache-2.0 or MIT.
- Resource Manager has no emulator. Its calls are tested against an HTTP fake
  (TLS, because azcore and MSAL refuse plain http for authenticated requests)
  that returns a recorded `servers.get` shape and also acts as the Entra
  authority, reached through `cloud.Egress.Azure` with a client secret. The
  integration job runs the same fake as a Go test server and the TLS PostgreSQL
  container as the "Azure" data plane.
- sqladmin has no emulator. Its calls are tested against an HTTP fake that
  returns recorded response shapes (`instances.get`, `users.insert` and
  `users.delete`, `operations.get`), reached through `cloud.Egress.GCP` with a
  service account key whose token endpoint is the fake too. The integration
  job runs the same fake as a Go test server and the TLS PostgreSQL container
  as the "Cloud SQL" data plane. Whether Cloud SQL accepts the operator's
  statements on the roles it creates (`ALTER ROLE`, `GRANT ... TO
  CURRENT_USER`, `REASSIGN OWNED`) and the IAM login itself are untested in CI
  ([NFR-0005](../nfrs/NFR-0005-cloud-credentials-and-egress.md)).
