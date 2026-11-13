# NFR-0004. Least-privilege RBAC, an audit trail, and no credential outside its Secret

## Status

Implemented

## Attribute

Security (access control, audit, secrets, plan rows EB-2, EB-4 and EB-9).

## Description

- The manager's ClusterRole lists exact resources and verbs: the group's kinds,
  their `status` and `finalizers`, `namespaces` (get, list, watch),
  `configmaps` (get), `secrets` and `events`; no wildcard. The manager's Secret
  cache holds only the Secrets labelled `app.kubernetes.io/managed-by`; the
  admin Secrets and CA bundles of servers are read from the API server on
  demand. A cluster server's admin Secret is read from the operator namespace
  only (`server.Resolve`); a namespaced server's from its own namespace, which
  is why the `secrets` rule is a ClusterRole rule.
- Each state change is an Event on the claim and a structured JSON log line (who,
  what, when, the server and the names of the changed fields, never their
  values), written by `lifecycle.Reconcile`.
- Admin and login passwords, and proxy URLs, never appear in Events, logs,
  conditions or status. A login's password is generated per issue
  (`crypto/rand`) and reaches only the binding Secret. Because PostgreSQL logs
  `ALTER ROLE ... PASSWORD` statements when `log_statement` is `ddl` or `all`,
  the server's statement logging must stay at `none` or `mod` for the admin
  role.

## Source

Plan rows EB-2, EB-4 and EB-9; [ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), sections 11 and 12.

## Verification

`config/rbac/role.yaml` is generated from the markers in `internal/app/rbac.go`
and checked for drift by the `generated-current` hook. `TestRoleLifecycleAndRotation`
and `TestDatabaseOnAnInvalidServer` assert that no Event or condition holds a
password or proxy credential.
