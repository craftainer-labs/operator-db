//go:build tools

// Package clouddeps pins the cloud SDK modules in go.mod that the operator does
// not import yet: `go mod tidy` drops a requirement nothing imports, and the
// dependency-only change NKdb-0 must keep them and their go.sum entries. The
// Cloud SQL Admin API (google.golang.org/api/sqladmin) is imported by
// internal/controller now. cloudsqlconn is the connector a consumer of an IAM
// binding uses; the operator itself logs in to Cloud SQL with the admin
// password (ADR 0005), so nothing here imports it until a decision says
// otherwise. azidentity is imported by internal/controller now (the Azure
// backend, NKdb-3a). The tools build
// tag keeps this file out of every build and test; tidy still reads it. Delete
// an entry once a package imports the same module.
package clouddeps

import (
	_ "cloud.google.com/go/cloudsqlconn"
)
