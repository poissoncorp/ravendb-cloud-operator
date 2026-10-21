// Package version holds the version the binaries were built as. The Makefile,
// the Dockerfile and the release workflow stamp it:
//
//	go build -ldflags "-X github.com/ravendb/ravendb-cloud-operator/internal/version.Version=1.2.3"
package version

// Version is "dev" in a build nobody stamped.
var Version = "dev"
