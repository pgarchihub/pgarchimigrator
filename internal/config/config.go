// Package config loads configuration from environment variables and a yaml
// file (FR-10: migration commands must be definable in yaml).
package config

import (
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/monitor"
	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
)

// Config gathers the entire application configuration in one place.
type Config struct {
	// DatabaseURL is read from an environment variable per TR-05 (a DSN
	// containing PGPASSWORD is never written to a file or a log).
	DatabaseURL string `yaml:"-"` // env: PGARCHIMIGRATOR_DATABASE_URL

	SmallTableRowThreshold int64 `yaml:"small_table_row_threshold"` // FR-01 default: 1_000_000

	Thresholds monitor.Thresholds `yaml:"throttle_thresholds"` // FR-05

	SwapConfig shadowflow.SwapConfig `yaml:"swap"` // FR-06

	RollbackWindow time.Duration `yaml:"rollback_window"` // FR-08a default: 10 * time.Minute

	MinPostgresVersion int `yaml:"min_postgres_version"` // TR-11, default: 12

	StateDBPath string `yaml:"state_db_path"` // SQLite file path (TR-13: single-instance)

	AuthDBPath string `yaml:"auth_db_path"` // SQLite file path for internal/auth's users/sessions — deliberately a SEPARATE file from StateDBPath, see internal/auth's package doc comment
}

// Default returns a Config populated with the defaults from the
// architecture/requirements docs.
func Default() Config {
	return Config{
		SmallTableRowThreshold: 1_000_000,
		Thresholds:             monitor.DefaultThresholds(),
		SwapConfig:             shadowflow.DefaultSwapConfig(),
		RollbackWindow:         10 * time.Minute,
		MinPostgresVersion:     12,
		StateDBPath:            "./pgarchimigrator-state.db",
		AuthDBPath:             "./pgarchimigrator-auth.db",
	}
}

// TODO: LoadFromFile(path string) (Config, error) — merges a yaml file on top of Default().
// TODO: LoadFromEnv() — reads environment variables such as PGARCHIMIGRATOR_DATABASE_URL.
