//go:build tools

// Package clouddeps pins the cloud SDK modules in go.mod before the cloud
// backends (NKdb-2, NKdb-3) import them: `go mod tidy` drops a
// requirement nothing imports, and the dependency-only change NKdb-0 must keep
// them and their go.sum entries. The tools build tag keeps this file out of
// every build and test; tidy still reads it. Delete this package once the
// backends import the same packages.
package clouddeps

import (
	_ "cloud.google.com/go/cloudsqlconn"
	_ "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	_ "google.golang.org/api/sqladmin/v1"
)
