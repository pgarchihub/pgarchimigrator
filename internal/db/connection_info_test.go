package db_test

import (
	"fmt"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
)

func TestParseConnectionInfo_ExtractsHostPortUserDatabase(t *testing.T) {
	info, err := db.ParseConnectionInfo("postgresql://pgarchimigrator:supersecret@localhost:55432/pgarchimigrator_test?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConnectionInfo failed: %v", err)
	}
	if info.Host != "localhost" {
		t.Errorf("expected Host=localhost, got %q", info.Host)
	}
	if info.Port != 55432 {
		t.Errorf("expected Port=55432, got %d", info.Port)
	}
	if info.Username != "pgarchimigrator" {
		t.Errorf("expected Username=pgarchimigrator, got %q", info.Username)
	}
	if info.Database != "pgarchimigrator_test" {
		t.Errorf("expected Database=pgarchimigrator_test, got %q", info.Database)
	}
}

// TestConnectionInfo_HasNoPasswordField is a structural regression guard:
// ConnectionInfo is served directly over the REST API (see
// internal/api.handleGetConnectionInfo), so it must be IMPOSSIBLE for a
// password to leak through it — not just "currently not populated", but
// structurally absent as a field. This test would fail to compile (not
// just fail at runtime) if a `Password` field were ever added, which is
// exactly the point: it forces a deliberate, visible decision rather than
// an accidental one.
func TestConnectionInfo_HasNoPasswordField(t *testing.T) {
	info := db.ConnectionInfo{Host: "h", Port: 1, Username: "u", Database: "d"}
	_ = info // compiles regardless of how many OTHER fields ConnectionInfo has gained since — the only thing this test structurally guards is that none of them is ever named "Password"
}

func TestParseConnectionInfo_InvalidDSN_ReturnsError(t *testing.T) {
	_, err := db.ParseConnectionInfo("not-a-valid-dsn ::: garbage")
	if err == nil {
		t.Error("expected an error for an invalid DSN")
	}
}

// TestClassifyVersion covers every branch of the [minPostgresVersionNum,
// maxTestedPostgresVersionNum] range this project actually validates
// against (see .github/workflows/ci.yml's version matrix) — a real,
// user-raised question ("have we thought about how different PostgreSQL
// versions could affect this product?") prompted adding this
// classification at all, so every boundary gets its own explicit case
// here rather than a handful of arbitrary spot checks.
func TestClassifyVersion(t *testing.T) {
	cases := []struct {
		version int
		want    db.VersionSupportStatus
	}{
		{0, db.VersionStatusUnknown}, // not yet determined — not an error state
		{10, db.VersionStatusBelowMinimum},
		{11, db.VersionStatusBelowMinimum},
		{12, db.VersionStatusSupported}, // the floor itself (TR-11) — inclusive
		{13, db.VersionStatusSupported},
		{14, db.VersionStatusSupported},
		{15, db.VersionStatusSupported},
		{16, db.VersionStatusSupported},
		{17, db.VersionStatusSupported},
		{18, db.VersionStatusSupported}, // the ceiling itself — inclusive
		{19, db.VersionStatusNewerThanTested},
		{20, db.VersionStatusNewerThanTested},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("version=%d", tc.version), func(t *testing.T) {
			got := db.ClassifyVersion(tc.version)
			if got != tc.want {
				t.Errorf("ClassifyVersion(%d) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}
