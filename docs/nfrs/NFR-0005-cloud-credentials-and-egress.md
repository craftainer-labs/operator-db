# NFR-0005. Cloud credentials stay out of output and every cloud call goes through the egress policy

## Status

Implemented for the AWS backend in password mode (NKdb-1a) and IAM mode
(NKdb-1b), for the GCP backend (Cloud SQL, NKdb-2) and for the Azure backend in
Password mode (NKdb-3a). The Azure Entra login and IAM mode (NKdb-3b) add to
its verification.

## Attribute

Security (credential handling, server-side request forgery, multi-tenancy;
plan Track NK, rows EB-2, EB-3, EB-5, EB-9 and EB-14).

## Description

- The operator's credentials to the cloud come from the SDK's default chain
  (`WorkloadIdentity`, the default on cluster servers) or from a Secret
  (`credentials.secretRef`). A namespaced server cannot use `WorkloadIdentity`:
  its tenant would otherwise act as the operator's cloud identity. A cluster
  server's Secret must be in the operator's namespace; the controller refuses
  any other (`CredentialsNamespaceForbidden`), because CEL cannot know it.
- Secret contents, access keys, ARNs and SDK error text never appear in a
  condition, Event, status field or log. Cloud failures are one fixed category
  each (`CredentialsUnavailable`, `TargetNotFound`, `EndpointOverrideForbidden`,
  `CloudError`).
- Every HTTP connection to a cloud API, STS included, dials through the egress
  policy of [NFR-0003](NFR-0003-restricted-egress-and-proxy.md) (`cloud.Egress`
  from lib-operator-go). `endpointOverride` exists only on cluster servers and
  is honoured only with `EGRESS_MODE=allow-private`; otherwise the server is
  `CloudResolved=False`, reason `EndpointOverrideForbidden`. It is for
  emulators, never production. The platform's own credential sources (the EKS
  Pod Identity agent, link-local) are the operator's environment's choice, not a
  tenant's.
- The operator never creates or edits its own cloud identity, trust policy or
  grants. Its AWS permissions are `rds:DescribeDBInstances` and
  `rds:DescribeDBClusters`. In IAM mode it builds no token and holds no
  `rds-db:connect`: the consumer's own role does, on the ARN in the binding.
  Its GCP permissions are `cloudsql.instances.get`, and for IAM claims
  `cloudsql.users.create` and `cloudsql.users.delete`; the consumer connects
  as its own identity through the Cloud SQL connector.
- An IAM-mode binding has no password and no `uri`; the role it names is
  created without a password and is the claim's only if it carries the claim's
  marker, so a tenant cannot take over or drop another role. On Cloud SQL
  the database user is named by the claim's `iam.principal`, so the marker is
  checked before `users.insert` and `users.delete` is called only for a role
  that carries it.
- A GCP key accepted from a Secret is a `service_account` key only, and the
  Cloud SQL Admin API client is the one `cloud.Egress.GCP` builds (token
  endpoint included); its error text, which names the project, the instance
  and the principal, is never kept.
- The Azure client is the one `cloud.Egress.Azure` builds (the Entra authority
  included, over https only); Resource Manager's error text, which names the
  subscription, the resource group and the server, is never kept. The operator's
  Azure permission is read access to the flexible server
  (`Microsoft.DBforPostgreSQL/flexibleServers/read`).

## Source

Plan Track NK and its Decision log rows of 2026-11-16 and 2026-11-17 (decisions
2, 3, 4 and 10 of the G-api);
[ADR 0005](../adrs/0005-cloud-backends.md);
ADR 0002 section 16 of lib-operator-go.

## Verification

- Unit (`internal/controller`): `TestCloudSecret` (namespace refusal, missing
  Secret), `TestDiscover` (strict mode refuses an override, malformed Secret),
  `TestDescribeFailuresAreCategories` (SDK text does not leak),
  `TestCloudServerBackendAndBinding`, `TestIAMRoleLifecycle` and
  `TestIAMRolePreconditions` (no password and the ARN in the binding, no
  rotation, drop); `TestApplyIAMRole` in `internal/pgclient` (quoting, the
  ownership check before any statement); CEL in `api/v1alpha1`
  (`TestProviderRules`).
- Unit, GCP (`internal/controller/gcp_test.go`, `internal/pgclient`):
  `TestDescribeCloudSQL` and `TestDescribeCloudSQLFailuresAreCategories`
  (recorded `instances.get` shapes; the API text does not leak),
  `TestDiscoverGCP` (the key's token reaches the API through `cloud.Egress.GCP`;
  strict mode refuses `endpointOverride`; a missing Secret and an
  `external_account` key are refused), `TestWaitOperation`,
  `TestGCPIAMRoleLifecycle` (marker before the grants, no password, the binding,
  no rotation, collision calls nothing, deletion), `TestIAMPrincipalChecks`,
  `TestApplyCloudSQLIAMRole` and `TestDropCloudSQLIAMRole` (quoting, ownership
  before `users.insert`/`users.delete`); CEL (`TestGCPProviderRules`,
  `TestIAMPrincipalRules`).
- Integration (job `cloudsql`): the Admin API fake as a Go test server and the TLS
  PostgreSQL at a private address (`TestCloudSQLProbeDiscoversAndConnects`,
  `TestCloudSQLIAMRole`).
- Integration (`.github/workflows/integration.yml`, job `rds`): moto server and
  the TLS PostgreSQL at private container addresses with `allow-private` in the
  test wiring (`TestRDSProbeDiscoversAndConnects`,
  `TestRDSProbeFailuresAreCategories`), and for IAM mode `TestRDSIAMRole`
  (login role, `rds_iam` membership, no password, privileges, drop),
  `TestRDSIAMRoleNameCollision` (a role that is not the claim's is left alone)
  and `TestRDSIAMResourceARN` (the ARN against what moto reports).
- Unit, Azure (`internal/controller/azure_test.go`): `TestDescribeAzure` and
  `TestDescribeAzureFailuresAreCategories` (a recorded `servers.get` shape
  through `cloud.Egress.Azure` with a fake Entra authority; the API text and
  the subscription do not leak; a refused client secret is
  `CredentialsUnavailable`), `TestDiscoverAzureClientErrors` (strict mode
  refuses `endpointOverride`, a missing or incomplete Secret),
  `TestProberDiscoversTheAzureAddress`, `TestAzurePasswordRole` (the binding;
  no Resource Manager call in Password mode; IAM refused); CEL
  (`TestAzureProviderRules`).
- Integration (job `azure`): the Resource Manager and Entra fake as a Go TLS
  test server and the TLS PostgreSQL at a private address
  (`TestAzureProbeDiscoversAndConnects`, `TestAzureProbeFailuresAreCategories`).
- **Untested in CI (Azure):** Resource Manager and Entra against the real
  services: that Azure accepts the operator's credentials, the workload identity
  chain (AKS), the shape of a real `servers.get` answer beyond the recorded one,
  and that the admin of a flexible server may run the Password-mode statements.
  Entra token validation, the Entra administrator login and `pgaadauth` belong
  to NKdb-3b and are untested in CI there too (no emulator).
- **Untested in CI (GCP):** sqladmin and IAM token validation against a real
  service: that Cloud SQL accepts the operator's credentials, the
  WorkloadIdentity chain (GKE), `users.insert` and `users.delete` with real
  operations, that the statements the operator runs on the roles Cloud SQL
  creates are permitted to the admin user, and IAM database authentication with
  the consumer's identity (there is no emulator for either).
- **Untested in CI (AWS):** that real AWS accepts the operator's credentials, the
  WorkloadIdentity chain (IRSA, Pod Identity), AssumeRole through `roleARN`, and
  RDS IAM token validation: whether RDS accepts a token for the login role and
  the `rds-db:connect` policy on the ARN in the binding (the test PostgreSQL has
  a stand-in `rds_iam` role and does not validate tokens). moto does not enforce
  IAM; there is no real-cloud lane.
