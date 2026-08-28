module github.com/pgarchihub/pgarchimigrator

// pgx v5.9.2 (see the require block below) transitively requires Go
// >= 1.25.0. This directive is kept in sync with that — see the Dockerfile
// build stage's comment for what breaks if it drifts out of sync again.
go 1.25

require (
	github.com/jackc/pgx/v5 v5.9.2
	github.com/spf13/cobra v1.8.0
	go.uber.org/zap v1.27.0
	modernc.org/sqlite v1.29.5
)

// NOTE: github.com/jackc/pglogrepl is used by internal/shadowflow/decoder.go
// and internal/shadowflow/replication.go but is deliberately NOT pinned to a
// specific version here — guessing a pseudo-version string risks an
// unresolvable "unknown revision" error that this sandboxed environment
// cannot verify against the real module proxy. Run `go mod tidy` (or
// `go get github.com/jackc/pglogrepl@latest`) on a machine with full
// internet access; it will add the correct require line automatically.
//
// NOTE: golang.org/x/crypto (bcrypt, used by internal/auth/password.go) is
// left unpinned for the same reason.
