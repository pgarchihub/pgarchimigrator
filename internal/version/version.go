// Package version holds the build-time version string for this binary.
//
// Set via `-ldflags "-X github.com/pgarchihub/pgarchimigrator/internal/version.Version=v0.1.0"`
// (see the Dockerfile's build stage) — deliberately never hardcoded per
// release in source, so a build can never ship a version string that
// doesn't match the git tag it was actually built from. Defaults to
// "dev" for a plain `go build`/`go run` without that flag, which is
// every local development build throughout this project.
package version

var Version = "dev"
