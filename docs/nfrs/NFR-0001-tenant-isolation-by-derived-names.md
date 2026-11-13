# NFR-0001. A claim reaches only the PostgreSQL objects derived from its own namespace and name

## Status

Implemented

## Attribute

Security (multi-tenancy, plan row EB-3).

## Description

The namespace is the tenant. The operator connects with an admin credential,
which PostgreSQL cannot confine to a tenant, so two things alone keep a claim
away from another tenant's objects:

- every database and role name is derived (`internal/pgclient/quote.go`:
  `DatabaseName`, `RoleBase`) with `naming.Tenant` from the claim's
  namespace and name, with the short hash (50 bits) because both objects carry
  the ownership marker; there is no field that sets a name;
- the marker `craftainer.dev/claim=<namespace>/<name>/<uid>` is kept in
  `COMMENT ON DATABASE` and `COMMENT ON ROLE`; an object whose marker names
  another claim is never read, changed or deleted and the claim gets
  `Synced=False`, reason `NameCollision`. Deleting the claim leaves such an
  object alone and releases the finalizer: a claim whose status records no
  create is released without contacting the server, and one that does is
  released over an object whose marker names another claim as if there were
  none;
- a cluster server serves only the namespaces its `allowedNamespaces`
  admits (`server.Resolve`), and a namespaced server only its own.

A DatabaseRole may use only a Database of its own namespace on the same server.

## Source

Plan row EB-3; [ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), sections 2 and 10.

## Verification

Unit and envtest suites, on every push: `TestNamesUseTheHashLengthOfADR0002`
(`internal/pgclient`), `TestDatabaseLifecycleRetain` and
`TestRoleLifecycleAndRotation` (collision leaves no statement),
`TestDatabaseNeverAdoptsByNameAlone`, `TestDatabaseDeleteSkipsAForeignDatabase`
and `TestRoleRecordsTheCreateAndSkipsForeignRolesOnDelete` (a foreign object is
neither dropped nor does it hold the finalizer),
`TestDatabaseWithoutAServerOrANamespace` (`NamespaceNotAllowed`),
`TestRoleNeedsAUsableDatabase` (`internal/controller`). Whether the claim
reaches only its own objects on a live server is checked by the PostgreSQL
integration lane (NO2b-2).
