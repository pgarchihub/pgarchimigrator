module github.com/pgarchihub/pgarchimigrator

// pgx v5.9.2 (see the require block below) transitively requires Go
// >= 1.25.0. This directive is kept in sync with that — see the Dockerfile
// build stage's comment for what breaks if it drifts out of sync again.
go 1.25.0

require (
	github.com/jackc/pglogrepl v0.0.0-20260824121319-4ae5c490f7ce
	github.com/jackc/pgx/v5 v5.9.2
	github.com/spf13/cobra v1.8.0
	golang.org/x/crypto v0.55.0
	modernc.org/sqlite v1.29.5
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgio v1.0.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.16 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.41.0 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.7.2 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
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
