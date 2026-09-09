// Command pgarchimigrator is the CLI entry point for pgArchiMigrator
// (Architecture Doc Section 5: "CLI (Cobra) + REST API").
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/db"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/ddlflow"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/preview"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/progress"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/reaper"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/shadowflow"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/typecompat"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/upgrade"
	"github.com/pgarchihub/pgarchimigrator/internal/api"
	"github.com/pgarchihub/pgarchimigrator/internal/auditlog"
	"github.com/pgarchihub/pgarchimigrator/internal/auth"
	"github.com/pgarchihub/pgarchimigrator/internal/config"
	"github.com/pgarchihub/pgarchimigrator/internal/ecosystem"
	"github.com/pgarchihub/pgarchimigrator/internal/entitlement"
	"github.com/pgarchihub/pgarchimigrator/internal/idempotency"
	"github.com/pgarchihub/pgarchimigrator/internal/migrationfile"
	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/serviceauth"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
	"github.com/pgarchihub/pgarchimigrator/internal/version"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "pgarchimigrator",
		Short: "Zero-downtime schema change tool for PostgreSQL",
	}

	root.AddCommand(newMigrateCmd())
	root.AddCommand(newApplyFileCmd())
	root.AddCommand(newPreviewFileCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newSweepCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newEcosystemCmd())
	root.AddCommand(newUpgradeCmd())
	root.AddCommand(newServeCmd()) // REST API (FR-09/FR-10, for the dashboard)
	root.AddCommand(newVersionCmd())

	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the pgarchimigrator version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version.Version)
			// Same PGARCHIMIGRATOR_EDITION resolution every other edition
			// check in this codebase uses (see entitlement.NewConfigChecker's
			// own doc comment) — this command never hardcodes "Community",
			// so a genuinely Enterprise-configured build reports its own
			// real edition here too, not a copy-pasted label.
			edition := entitlement.NewConfigChecker(os.Getenv("PGARCHIMIGRATOR_EDITION")).Edition()
			// Capitalizes "community"/"enterprise" for display — done
			// manually rather than via the now-deprecated strings.Title,
			// which exists purely to avoid a linter warning for a
			// one-line job on a fixed, ASCII-only set of values.
			editionStr := string(edition)
			if editionStr != "" {
				editionStr = strings.ToUpper(editionStr[:1]) + editionStr[1:]
			}
			fmt.Printf("Edition: %s\n", editionStr)
		},
	}
}

// wiring groups everything StartMigration/RollbackMigration need — built
// once per CLI invocation from the PGARCHIMIGRATOR_DATABASE_URL environment
// variable (TR-05: the DSN, which may contain a password, is never read
// from a config file or flag).
type wiring struct {
	pool        *pgxpool.Pool
	store       *state.SQLiteStore
	auditWriter *auditlog.FileWriter
	orch        *orchestrator.Orchestrator
	connInfo    db.ConnectionInfo
	// entitlement is exposed here (not just consumed internally) so a
	// future internal/api wiring point — e.g. a dashboard badge showing
	// which edition is running — can read it without needing its own,
	// separate PGARCHIMIGRATOR_EDITION lookup. Not yet consumed
	// anywhere outside this file; see docs/ecosystem/ARCHITECTURE.md
	// for the full rollout plan.
	entitlement entitlement.Checker
}

func (w *wiring) Close() {
	if w.store != nil {
		w.store.Close()
	}
	if w.auditWriter != nil {
		w.auditWriter.Close()
	}
	if w.pool != nil {
		w.pool.Close()
	}
}

func buildWiring(ctx context.Context, stateDBPath string) (*wiring, error) {
	dsn := os.Getenv("PGARCHIMIGRATOR_DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("PGARCHIMIGRATOR_DATABASE_URL is not set (see the setup guide, Section 4)")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the database: %w", err)
	}

	// Parsed once here (not re-parsed per-request in internal/api) since
	// the DSN never changes for the lifetime of the process — see
	// db.ConnectionInfo's doc comment for why this is safe to hand to the
	// REST API layer (no password field exists on the type at all).
	// Failure to parse is deliberately non-fatal: the connection itself
	// already succeeded above using this same dsn, so a parse error here
	// would be surprising, but the New Migration screen's "connected to"
	// banner is a nice-to-have, not something worth failing startup over.
	connInfo, err := db.ParseConnectionInfo(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not parse connection info for display purposes: %v\n", err)
	}
	// Same non-fatal-on-failure reasoning as ParseConnectionInfo just
	// above — this is display-only (and, separately, the general
	// TR-11 gate wired into orch.VersionCheck below is what actually
	// blocks an unsupported version; this variable feeds the UI, not
	// enforcement).
	if versionNum, versionString, err := db.FetchPostgresVersion(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not determine PostgreSQL version for display purposes: %v\n", err)
	} else {
		connInfo.PostgresVersion = versionNum
		connInfo.PostgresVersionString = versionString
		connInfo.VersionSupportStatus = db.ClassifyVersion(versionNum)
	}

	store, err := state.NewSQLiteStore(stateDBPath)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to open the state database (%s): %w", stateDBPath, err)
	}

	auditPath := os.Getenv("PGARCHIMIGRATOR_AUDIT_LOG_PATH")
	if auditPath == "" {
		auditPath = "./pgarchimigrator-audit.jsonl"
	}
	auditWriter, err := auditlog.NewFileWriter(auditPath)
	if err != nil {
		// Audit logging is important (TR-07) but must not block the tool
		// from working at all — degrade to no audit logging rather than
		// failing every command if, say, the directory isn't writable.
		fmt.Fprintf(os.Stderr, "warning: audit logging disabled: %v\n", err)
	}

	preflighter := db.NewPgxPreflighter(pool)
	replicationDSN := shadowflow.ReplicationDSN(dsn)

	// Ecosystem integration — see docs/ecosystem/ARCHITECTURE.md for the
	// full design. Both are opt-in and default to "off"/"community"
	// specifically so upgrading to a build that includes this layer
	// never silently changes an existing deployment's behavior: no
	// events are published unless PGARCHIMIGRATOR_ECOSYSTEM_EVENTS_ENABLED
	// is explicitly set to "true".
	entitlementChecker := entitlement.NewConfigChecker(os.Getenv("PGARCHIMIGRATOR_EDITION"))

	// effectiveStore is what every flow/orchestrator below actually
	// receives — either the plain SQLite store, or that same store
	// wrapped with ecosystem.Store when event publishing is enabled.
	// Deliberately kept as the state.Store INTERFACE (not the concrete
	// *state.SQLiteStore type wiring.store holds for its own Close()
	// call below) — ddlflow/shadowflow/orchestrator only ever depend on
	// the interface, so this substitution is invisible to all three.
	var effectiveStore state.Store = store
	if os.Getenv("PGARCHIMIGRATOR_ECOSYSTEM_EVENTS_ENABLED") == "true" {
		instanceID := os.Getenv("PGARCHIMIGRATOR_INSTANCE_ID")
		if instanceID == "" {
			if hostname, err := os.Hostname(); err == nil {
				instanceID = hostname
			}
		}
		// LogPublisher today — see its own doc comment for why this is
		// a genuinely working integration for a self-hosted operator,
		// not a placeholder, even before a real message-broker/webhook
		// Publisher exists.
		effectiveStore = ecosystem.NewStore(store, ecosystem.LogPublisher{}, entitlementChecker, instanceID, version.Version)
	}

	flowFor := func(strat strategy.Strategy) (orchestrator.Flow, error) {
		switch strat {
		case strategy.StrategyDirectDDL, strategy.StrategyExpandBackfill:
			return ddlflow.New(pool, effectiveStore), nil
		case strategy.StrategyShadowTable:
			return shadowflow.New(pool, replicationDSN, effectiveStore, preflighter), nil
		default:
			return nil, fmt.Errorf("no flow registered for strategy %s", strat)
		}
	}

	tableStats := func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
		raw, err := db.FetchTableStats(ctx, pool, schema, table)
		if err != nil {
			return strategy.TableStats{}, err
		}
		return strategy.TableStats{
			EstimatedRowCount: raw.EstimatedRowCount,
			IsPartitioned:     raw.IsPartitioned,
			HasPrimaryKey:     raw.HasPrimaryKey,
			ReplicaIdentity:   raw.ReplicaIdentity,
		}, nil
	}

	orch := orchestrator.New(effectiveStore, flowFor, tableStats)
	if auditWriter != nil {
		orch.AuditWriter = auditWriter
	}
	orch.VersionCheck = func(ctx context.Context) error {
		return db.ValidateMinimumVersion(ctx, pool)
	}

	return &wiring{
		pool:        pool,
		store:       store,
		auditWriter: auditWriter,
		orch:        orch,
		connInfo:    connInfo,
		entitlement: entitlementChecker,
	}, nil
}

// currentActor identifies who is running the CLI, for the audit log
// (TR-07). PGARCHIMIGRATOR_ACTOR lets CI/CD pipelines identify themselves by name
// (e.g. "github-actions", "gitlab-ci-orders-migration") instead of
// whatever OS account the runner happens to use.
func currentActor() string {
	if a := os.Getenv("PGARCHIMIGRATOR_ACTOR"); a != "" {
		return a
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func newMigrateCmd() *cobra.Command {
	var (
		stateDBPath             string
		schemaName              string
		tableName               string
		columnName              string
		operationStr            string
		columnType              string
		defaultValue            string
		isVolatile              bool
		strategyOverride        string
		indexName               string
		constraintName          string
		checkExpression         string
		newColumnName           string
		newTableName            string
		referencedTable         string
		referencedColumn        string
		onDelete                string
		generatedExpr           string
		partitionColumn         string
		partitionStrategy       string
		partitionBoundsRaw      string
		partitionIncludeDefault bool
		partitionInterval       string
		partitionRuleFrom       string
		partitionRuleTo         string
		dryRun                  bool
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Start a schema-change migration (FR-01..FR-04)",
		RunE: func(cmd *cobra.Command, args []string) error {
			op := strategy.Operation(operationStr)

			// Populated inside the PARTITION_TABLE case below (either
			// directly from --partition-bounds, or expanded from a rule)
			// — declared here so it's in scope when the final
			// strategy.ColumnChange gets built further down.
			var partitionBoundsJSON string

			// --column, --index-name, and --constraint-name/--check-expression
			// are each required for some operations but not others, which
			// cobra's MarkFlagRequired can't express (it's all-or-nothing
			// per flag) — validated here instead, following the same
			// manual-validation pattern already used by `auth create-admin`.
			switch op {
			case strategy.OpDropIndex:
				if indexName == "" {
					return fmt.Errorf("--index-name is required for DROP_INDEX")
				}
			case strategy.OpAddIndex:
				if columnName == "" {
					return fmt.Errorf("--column is required for ADD_INDEX")
				}
			case strategy.OpAddConstraint:
				if constraintName == "" {
					return fmt.Errorf("--constraint-name is required for ADD_CONSTRAINT")
				}
				if checkExpression == "" {
					return fmt.Errorf("--check-expression is required for ADD_CONSTRAINT")
				}
			case strategy.OpSetNotNull:
				if columnName == "" {
					return fmt.Errorf("--column is required for SET_NOT_NULL")
				}
			case strategy.OpRenameColumn:
				if columnName == "" {
					return fmt.Errorf("--column (the existing name) is required for RENAME_COLUMN")
				}
				if newColumnName == "" {
					return fmt.Errorf("--new-column-name is required for RENAME_COLUMN")
				}
			case strategy.OpRenameTable:
				// Deliberately does NOT fall into the default case's
				// "--column is required" check below — the one
				// operation here that acts on the table itself, not any
				// particular column.
				if newTableName == "" {
					return fmt.Errorf("--new-table-name is required for RENAME_TABLE")
				}
			case strategy.OpAddForeignKey:
				if columnName == "" {
					return fmt.Errorf("--column (the local column) is required for ADD_FOREIGN_KEY")
				}
				if constraintName == "" {
					return fmt.Errorf("--constraint-name is required for ADD_FOREIGN_KEY")
				}
				if referencedTable == "" || referencedColumn == "" {
					return fmt.Errorf("--referenced-table and --referenced-column are both required for ADD_FOREIGN_KEY")
				}
			case strategy.OpAddGeneratedColumn:
				if columnName == "" {
					return fmt.Errorf("--column is required for ADD_GENERATED_COLUMN")
				}
				if columnType == "" {
					return fmt.Errorf("--type is required for ADD_GENERATED_COLUMN")
				}
				if generatedExpr == "" {
					return fmt.Errorf("--generated-expression is required for ADD_GENERATED_COLUMN")
				}
			case strategy.OpPartitionTable:
				if partitionColumn == "" {
					return fmt.Errorf("--partition-column is required for PARTITION_TABLE")
				}
				if err := strategy.ValidatePartitionStrategy(partitionStrategy); err != nil {
					return err
				}

				var bounds []strategy.PartitionBound
				if partitionBoundsRaw != "" {
					if err := json.Unmarshal([]byte(partitionBoundsRaw), &bounds); err != nil {
						return fmt.Errorf("--partition-bounds is not valid JSON: %w", err)
					}
				}
				if len(bounds) == 0 {
					if partitionStrategy != "RANGE" {
						return fmt.Errorf("--partition-bounds is required for LIST partitioning (no rule-based shortcut exists for it)")
					}
					if partitionInterval == "" || partitionRuleFrom == "" || partitionRuleTo == "" {
						return fmt.Errorf("either --partition-bounds, or all of --partition-interval/--partition-rule-from/--partition-rule-to, is required for PARTITION_TABLE")
					}
					expanded, err := strategy.ExpandPartitionRule(partitionInterval, partitionRuleFrom, partitionRuleTo, tableName)
					if err != nil {
						return err
					}
					bounds = expanded
				}

				boundsJSON, err := json.Marshal(bounds)
				if err != nil {
					return fmt.Errorf("failed to encode partition bounds: %w", err)
				}
				partitionBoundsJSON = string(boundsJSON)
			default: // ADD_COLUMN, DROP_COLUMN, ALTER_COLUMN_TYPE
				if columnName == "" {
					return fmt.Errorf("--column is required for %s", op)
				}
			}

			w, err := buildWiring(cmd.Context(), stateDBPath)
			if err != nil {
				return err
			}
			defer w.Close()

			// Automatic type-compatibility detection: for ALTER_COLUMN_TYPE
			// requests, check whether this specific old-type -> new-type
			// change is one of engines/postgresql/typecompat's curated "free" cases
			// (PostgreSQL applies it as metadata-only, no table rewrite).
			// Deliberately skipped entirely when the user gave an explicit
			// --strategy override — an explicit choice always wins, this
			// detection only fills in the answer when they didn't say.
			typeCompatible := false
			if op == strategy.OpAlterType && strategyOverride == "" {
				currentType, err := typecompat.CurrentColumnType(cmd.Context(), w.pool, schemaName, tableName, columnName)
				if err != nil {
					return fmt.Errorf("failed to determine the column's current type for compatibility detection: %w", err)
				}
				typeCompatible = typecompat.IsCompatible(currentType, columnType)
			}

			req := orchestrator.MigrationRequest{
				SchemaName: schemaName,
				TableName:  tableName,
				Change: strategy.ColumnChange{
					Operation:                op,
					ColumnName:               columnName,
					NewType:                  columnType,
					DefaultValue:             defaultValue,
					IsVolatileDefault:        isVolatile,
					IndexName:                indexName,
					ConstraintName:           constraintName,
					CheckExpression:          checkExpression,
					NewColumnName:            newColumnName,
					NewTableName:             newTableName,
					ReferencedTable:          referencedTable,
					ReferencedColumn:         referencedColumn,
					OnDelete:                 onDelete,
					GeneratedExpression:      generatedExpr,
					PartitionColumn:          partitionColumn,
					PartitionStrategy:        partitionStrategy,
					PartitionBoundsJSON:      partitionBoundsJSON,
					PartitionIncludeDefault:  partitionIncludeDefault,
					TypeConversionCompatible: typeCompatible,
				},
				StrategyOverride: strategy.Strategy(strategyOverride),
				Actor:            currentActor(),
			}

			if dryRun {
				report, err := preview.Generate(cmd.Context(), w.pool, w.orch.TableStats, req)
				if err != nil {
					return fmt.Errorf("dry run failed: %w", err)
				}
				fmt.Print(report.Render())
				return nil
			}

			job, err := w.orch.StartMigration(cmd.Context(), req)
			if job != nil {
				fmt.Print(progress.Compute(job).Render())
			}
			if err != nil {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&stateDBPath, "state-db", config.Default().StateDBPath, "path to the SQLite state database")
	cmd.Flags().StringVar(&schemaName, "schema", "public", "target schema name")
	cmd.Flags().StringVar(&tableName, "table", "", "target table name (required)")
	cmd.Flags().StringVar(&columnName, "column", "", "target column name (required for ADD_COLUMN, DROP_COLUMN, ALTER_COLUMN_TYPE, ADD_INDEX, SET_NOT_NULL, RENAME_COLUMN, ADD_FOREIGN_KEY, ADD_GENERATED_COLUMN — the existing name for RENAME_COLUMN, the local column for ADD_FOREIGN_KEY, the new column's name for ADD_GENERATED_COLUMN)")
	cmd.Flags().StringVar(&operationStr, "operation", "", "ADD_COLUMN, DROP_COLUMN, ALTER_COLUMN_TYPE, ADD_INDEX, DROP_INDEX, SET_NOT_NULL, ADD_CONSTRAINT, RENAME_COLUMN, RENAME_TABLE, ADD_FOREIGN_KEY, ADD_GENERATED_COLUMN, or PARTITION_TABLE (required)")
	cmd.Flags().StringVar(&columnType, "type", "", "new column type (required for ALTER_COLUMN_TYPE, or the type of the column being added)")
	cmd.Flags().StringVar(&defaultValue, "default", "", "default value expression for ADD_COLUMN (e.g. \"'active'\" or \"now()\")")
	cmd.Flags().BoolVar(&isVolatile, "volatile-default", false, "set if --default is a volatile expression (e.g. now()), triggering Expand & Backfill")
	cmd.Flags().StringVar(&strategyOverride, "strategy", "", "override the automatic strategy decision (DIRECT_DDL, EXPAND_BACKFILL, SHADOW_TABLE)")
	cmd.Flags().StringVar(&indexName, "index-name", "", "index name for ADD_INDEX (optional, auto-generated as idx_<table>_<column> if omitted) or DROP_INDEX (required)")
	cmd.Flags().StringVar(&constraintName, "constraint-name", "", "constraint name for SET_NOT_NULL (optional, auto-generated if omitted), ADD_CONSTRAINT (required), or ADD_FOREIGN_KEY (required)")
	cmd.Flags().StringVar(&checkExpression, "check-expression", "", "CHECK(...) expression body for ADD_CONSTRAINT, e.g. \"price > 0\" (required)")
	cmd.Flags().StringVar(&newColumnName, "new-column-name", "", "the new name for RENAME_COLUMN (required) — see the command's long help for why this doesn't do a plain ALTER TABLE RENAME")
	cmd.Flags().StringVar(&newTableName, "new-table-name", "", "the new name for RENAME_TABLE (required) — like RENAME_COLUMN, leaves a compatibility view under the old name rather than an instant, breaking rename")
	cmd.Flags().StringVar(&referencedTable, "referenced-table", "", "the table the foreign key references, in the same schema (required for ADD_FOREIGN_KEY)")
	cmd.Flags().StringVar(&referencedColumn, "referenced-column", "", "the column the foreign key references — usually a primary key (required for ADD_FOREIGN_KEY)")
	cmd.Flags().StringVar(&onDelete, "on-delete", "", "ON DELETE action for ADD_FOREIGN_KEY: CASCADE, SET NULL, SET DEFAULT, RESTRICT, or NO ACTION (optional — defaults to PostgreSQL's own NO ACTION)")
	cmd.Flags().StringVar(&generatedExpr, "generated-expression", "", "the expression for ADD_GENERATED_COLUMN, e.g. \"price * quantity\" (required) — a real native GENERATED column on a small table, or a trigger-kept-in-sync plain column on a large one; see the command's long help")
	cmd.Flags().StringVar(&partitionColumn, "partition-column", "", "the column to partition by (required for PARTITION_TABLE)")
	cmd.Flags().StringVar(&partitionStrategy, "partition-strategy", "", "RANGE or LIST (required for PARTITION_TABLE)")
	cmd.Flags().StringVar(&partitionBoundsRaw, "partition-bounds", "", `explicit partition bounds as JSON, e.g. '[{"name":"orders_eu","values":["DE","FR"]}]' for LIST or '[{"name":"orders_2024_01","from":"2024-01-01","to":"2024-02-01"}]' for RANGE — required for LIST; for RANGE, an alternative to --partition-interval/--partition-rule-from/--partition-rule-to`)
	cmd.Flags().BoolVar(&partitionIncludeDefault, "partition-include-default", false, "add a DEFAULT partition catching any row outside the explicit bounds (recommended unless your bounds are certainly exhaustive)")
	cmd.Flags().StringVar(&partitionInterval, "partition-interval", "", "RANGE only: daily, monthly, or yearly — generates evenly-spaced partitions between --partition-rule-from and --partition-rule-to instead of listing them out via --partition-bounds")
	cmd.Flags().StringVar(&partitionRuleFrom, "partition-rule-from", "", "RANGE rule-based shortcut: start date (YYYY-MM-DD), used with --partition-interval")
	cmd.Flags().StringVar(&partitionRuleTo, "partition-rule-to", "", "RANGE rule-based shortcut: end date (YYYY-MM-DD), used with --partition-interval")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview the strategy, the SQL that would run, and any pre-flight warnings — makes no changes")
	_ = cmd.MarkFlagRequired("table")
	_ = cmd.MarkFlagRequired("operation")

	return cmd
}

// newApplyFileCmd implements "Migration as Code" — see
// internal/migrationfile's own package doc comment for the full design.
// Applies every migration in a directory (or a single file), in
// filename order, skipping any whose ID (see MigrationFile.ID) already
// belongs to a COMPLETED job — idempotent by design, so re-running this
// command (e.g. on every deploy, matching how Flyway/golang-migrate are
// typically invoked in a CI/CD pipeline) only ever applies what's
// genuinely new.
func newApplyFileCmd() *cobra.Command {
	var (
		stateDBPath string
		dir         string
		file        string
	)

	cmd := &cobra.Command{
		Use:   "apply-file",
		Short: "Apply one or more migrations defined as JSON files (Migration as Code)",
		Long: `Apply one or more migrations defined as JSON files.

Reads migration definitions from --dir (every *.json file, applied in
filename order — use a numeric or timestamp prefix like "001_..." or
"20260826_..." to control ordering) or a single --file. Each migration's
"id" field is checked against already-COMPLETED jobs first — if one with
this exact id has already succeeded, it's skipped, so this command is
safe to run repeatedly (e.g. as a deploy-time step) without re-applying
anything.

See docs/migration-as-code.md for the file format and a worked example,
and .github/workflows/migration-preview.yml for the companion CI check
that runs "preview-file" (not this command — preview-file never touches
the database) automatically on pull requests that change migration
files.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			migrations, err := loadMigrationFiles(dir, file)
			if err != nil {
				return err
			}

			w, err := buildWiring(cmd.Context(), stateDBPath)
			if err != nil {
				return err
			}
			defer w.Close()

			applied, err := alreadyAppliedMigrationIDs(cmd.Context(), w.store)
			if err != nil {
				return fmt.Errorf("failed to check already-applied migrations: %w", err)
			}

			for _, m := range migrations {
				if applied[m.ID] {
					fmt.Printf("SKIP  %s (already applied)\n", m.ID)
					continue
				}

				fmt.Printf("APPLY %s", m.ID)
				if m.Description != "" {
					fmt.Printf(" — %s", m.Description)
				}
				fmt.Println()

				req, err := m.ToMigrationRequest(currentActor())
				if err != nil {
					return fmt.Errorf("migration %q: %w", m.ID, err)
				}
				job, err := w.orch.StartMigration(cmd.Context(), req)
				if job != nil {
					fmt.Print(progress.Compute(job).Render())
				}
				if err != nil {
					// Deliberately stops here rather than continuing to
					// the next file — migrations in a directory often
					// have real ordering dependencies (a later one
					// might reference a column an earlier one adds), so
					// silently skipping past a failure and continuing
					// could leave things in a confusing, partially-
					// applied state that's harder to diagnose than
					// simply stopping at the first problem.
					return fmt.Errorf("migration %q failed: %w", m.ID, err)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&stateDBPath, "state-db", config.Default().StateDBPath, "path to the SQLite state database")
	cmd.Flags().StringVar(&dir, "dir", "", "directory of migration JSON files to apply, in filename order (either this or --file is required)")
	cmd.Flags().StringVar(&file, "file", "", "a single migration JSON file to apply (either this or --dir is required)")

	return cmd
}

// newPreviewFileCmd is apply-file's read-only counterpart — generates a
// preview.Report (the same dry-run output "migrate --dry-run" produces)
// for every migration in a directory or file, WITHOUT ever calling
// StartMigration, so it's safe to run against a real database with no
// risk of making a change. This is what
// .github/workflows/migration-preview.yml actually invokes on every
// pull request that touches migration files — see that workflow for how
// its output becomes a PR comment.
func newPreviewFileCmd() *cobra.Command {
	var (
		stateDBPath string
		dir         string
		file        string
	)

	cmd := &cobra.Command{
		Use:   "preview-file",
		Short: "Preview one or more migrations defined as JSON files, without applying them",
		RunE: func(cmd *cobra.Command, args []string) error {
			migrations, err := loadMigrationFiles(dir, file)
			if err != nil {
				return err
			}

			w, err := buildWiring(cmd.Context(), stateDBPath)
			if err != nil {
				return err
			}
			defer w.Close()

			for _, m := range migrations {
				fmt.Printf("=== %s", m.ID)
				if m.Description != "" {
					fmt.Printf(" — %s", m.Description)
				}
				fmt.Println(" ===")

				req, err := m.ToMigrationRequest(currentActor())
				if err != nil {
					fmt.Printf("PREVIEW FAILED: %v\n\n", err)
					continue
				}
				report, err := preview.Generate(cmd.Context(), w.pool, w.orch.TableStats, req)
				if err != nil {
					// Unlike apply-file, a preview failure for one
					// migration doesn't block previewing the rest —
					// this command's whole purpose is showing a
					// reviewer the complete picture, and one migration
					// referencing a table/column that doesn't exist YET
					// (because an earlier migration in the same PR
					// would create it first) is a normal, expected
					// situation to just report and move past, not a
					// reason to stop.
					fmt.Printf("PREVIEW FAILED: %v\n\n", err)
					continue
				}
				fmt.Print(report.Render())
				fmt.Println()
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&stateDBPath, "state-db", config.Default().StateDBPath, "path to the SQLite state database")
	cmd.Flags().StringVar(&dir, "dir", "", "directory of migration JSON files to preview (either this or --file is required)")
	cmd.Flags().StringVar(&file, "file", "", "a single migration JSON file to preview (either this or --dir is required)")

	return cmd
}

// loadMigrationFiles is the shared --dir/--file resolution logic between
// apply-file and preview-file.
func loadMigrationFiles(dir, file string) ([]migrationfile.MigrationFile, error) {
	if dir == "" && file == "" {
		return nil, fmt.Errorf("either --dir or --file is required")
	}
	if dir != "" && file != "" {
		return nil, fmt.Errorf("--dir and --file are mutually exclusive")
	}
	if dir != "" {
		return migrationfile.LoadDir(dir)
	}
	m, err := migrationfile.LoadFile(file)
	if err != nil {
		return nil, err
	}
	return []migrationfile.MigrationFile{m}, nil
}

// alreadyAppliedMigrationIDs returns the set of migration file IDs (see
// MigrationFile.ID) that already correspond to a COMPLETED job — see
// migrationfile.MigrationFile.ToMigrationRequest's own doc comment for
// why the file's ID becomes the job's Name, which is what this checks
// against.
func alreadyAppliedMigrationIDs(ctx context.Context, store *state.SQLiteStore) (map[string]bool, error) {
	jobs, err := store.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	applied := make(map[string]bool)
	for _, j := range jobs {
		if j.Name != "" && j.Phase == state.PhaseCompleted {
			applied[j.Name] = true
		}
	}
	return applied, nil
}

func newRollbackCmd() *cobra.Command {
	var stateDBPath string
	cmd := &cobra.Command{
		Use:   "rollback [job-id]",
		Short: "Roll back an in-progress or completed migration (FR-07/FR-08/FR-08a)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := buildWiring(cmd.Context(), stateDBPath)
			if err != nil {
				return err
			}
			defer w.Close()

			job, err := w.orch.RollbackMigration(cmd.Context(), args[0], currentActor())
			if job != nil {
				fmt.Print(progress.Compute(job).Render())
			}
			return err
		},
	}
	cmd.Flags().StringVar(&stateDBPath, "state-db", config.Default().StateDBPath, "path to the SQLite state database")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var stateDBPath string
	cmd := &cobra.Command{
		Use:   "status [job-id]",
		Short: "Show the progress of a migration (FR-04, US-03)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := state.NewSQLiteStore(stateDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the state database (%s): %w", stateDBPath, err)
			}
			defer store.Close()

			job, err := store.Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to load job %q: %w", args[0], err)
			}

			report := progress.Compute(job)
			fmt.Print(report.Render())
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDBPath, "state-db", config.Default().StateDBPath, "path to the SQLite state database")
	return cmd
}

// newListCmd lists every known job so the user can find an exact job ID
// instead of reconstructing one from scrollback or from database object
// names (e.g. a shadow table's inherited index name) — a mistake that
// risks acting on the wrong, unrelated job entirely.
func newListCmd() *cobra.Command {
	var stateDBPath string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all known migration jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := state.NewSQLiteStore(stateDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the state database (%s): %w", stateDBPath, err)
			}
			defer store.Close()

			jobs, err := store.ListAll(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to list jobs: %w", err)
			}
			if len(jobs) == 0 {
				fmt.Println("No jobs found.")
				return nil
			}
			for _, job := range jobs {
				fmt.Printf("%s  %-16s %-14s %s.%s\n", job.ID, job.Strategy, job.Phase, job.SchemaName, job.TableName)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDBPath, "state-db", config.Default().StateDBPath, "path to the SQLite state database")
	return cmd
}

// newSweepCmd runs a single reaper pass on demand: orphan/crash cleanup
// (ScanOnce) plus completing migrations whose FR-08a rollback window has
// expired (SweepExpiredRollbackWindows). In production this normally runs
// automatically via reaper.Run's periodic loop (see engines/postgresql/reaper); this
// command exists for manual/on-demand cleanup and for cron-based
// deployments that prefer an external scheduler over a long-running
// process.
func newSweepCmd() *cobra.Command {
	var stateDBPath string
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Run one orphan-cleanup and expired-rollback-window pass (Architecture Doc Section 3.3)",
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := buildWiring(cmd.Context(), stateDBPath)
			if err != nil {
				return err
			}
			defer w.Close()

			r := reaper.New(w.store, w.pool)

			scanResult, scanErr := r.ScanOnce(cmd.Context())
			if scanResult != nil {
				fmt.Printf("Orphan scan: %d job(s) scanned, %d slot(s) dropped, %d shadow table(s) dropped\n",
					scanResult.JobsScanned, len(scanResult.SlotsDropped), len(scanResult.ShadowTablesDropped))
				for _, e := range scanResult.Errors {
					fmt.Printf("  scan error: %v\n", e)
				}
			}

			sweepResult, sweepErr := r.SweepExpiredRollbackWindows(cmd.Context())
			if sweepResult != nil {
				fmt.Printf("Rollback-window sweep: %d job(s) completed\n", sweepResult.JobsSwept)
				for _, e := range sweepResult.Errors {
					fmt.Printf("  sweep error: %v\n", e)
				}
			}

			if scanErr != nil {
				return fmt.Errorf("orphan scan failed: %w", scanErr)
			}
			if sweepErr != nil {
				return fmt.Errorf("rollback-window sweep failed: %w", sweepErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDBPath, "state-db", config.Default().StateDBPath, "path to the SQLite state database")
	return cmd
}

func newServeCmd() *cobra.Command {
	var (
		stateDBPath   string
		authDBPath    string
		upgradeDBPath string
		addr          string
		autoSweep     bool
		secureCookies bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the REST API + dashboard server (Architecture Doc Section 5)",
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := buildWiring(cmd.Context(), stateDBPath)
			if err != nil {
				return err
			}
			defer w.Close()

			authStore, err := auth.NewSQLiteStore(authDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the auth database (%s): %w", authDBPath, err)
			}
			defer authStore.Close()
			authService := auth.NewService(authStore)

			// Service-to-service (OAuth2 client-credentials) auth for
			// ecosystem callers — see docs/ecosystem/ARCHITECTURE.md.
			// Deliberately shares authDBPath with the human-auth store
			// just above rather than needing its own --serviceauth-db
			// flag: internal/serviceauth's own SQLiteStore doc comment
			// notes this is a fine wiring-level choice (SQLite has no
			// objection to multiple unrelated table sets in one file),
			// and it's one fewer path for an operator to configure.
			serviceAuthStore, err := serviceauth.NewSQLiteStore(authDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the service-auth database (%s): %w", authDBPath, err)
			}
			defer serviceAuthStore.Close()
			serviceAuthService := serviceauth.NewService(serviceAuthStore)

			// PostgreSQL major-version upgrade (see
			// docs/ecosystem/ARCHITECTURE.md's own "PostgreSQL
			// major-version upgrade" section) — its own separate
			// SQLite file, same "isolate this write path" reasoning
			// as engines/postgresql/upgrade.SQLiteStore's own doc comment.
			// StaticConnectionProvider is today's only
			// ConnectionProvider — see that type's own doc comment for
			// why Enterprise/Cloud can later swap in a dynamic one
			// without any handler code changing.
			upgradeStore, err := upgrade.NewSQLiteStore(upgradeDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the upgrade database (%s): %w", upgradeDBPath, err)
			}
			defer upgradeStore.Close()

			// Same opt-in ecosystem-events wrapping buildWiring already
			// applies to the migration store (effectiveStore) — see
			// that function's own comment on PGARCHIMIGRATOR_ECOSYSTEM_EVENTS_ENABLED.
			// w.entitlement is buildWiring's own already-constructed
			// entitlement.Checker (see wiring's own doc comment on that
			// field), reused here rather than building a second one, so
			// the migration store and the upgrade store always agree on
			// which edition this instance is running as.
			var effectiveUpgradeStore upgrade.Store = upgradeStore
			if os.Getenv("PGARCHIMIGRATOR_ECOSYSTEM_EVENTS_ENABLED") == "true" {
				instanceID := os.Getenv("PGARCHIMIGRATOR_INSTANCE_ID")
				if instanceID == "" {
					if hostname, err := os.Hostname(); err == nil {
						instanceID = hostname
					}
				}
				effectiveUpgradeStore = ecosystem.NewUpgradeStore(upgradeStore, ecosystem.LogPublisher{}, w.entitlement, instanceID, version.Version)
			}

			// Idempotency-Key support (AC-PF-003 §12.2/AOL-STD-API-001
			// §5.5) — same "shares authDBPath, one fewer path to
			// configure" reasoning as serviceAuthStore just above.
			idempotencyStore, err := idempotency.NewSQLiteStore(authDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the idempotency database (%s): %w", authDBPath, err)
			}
			defer idempotencyStore.Close()

			r := reaper.New(w.store, w.pool)

			bgCtx, cancelBg := context.WithCancel(context.Background())
			defer cancelBg()

			if autoSweep {
				// Closes the gap noted in earlier sessions: previously
				// reaper.Run only ran via tests or the one-shot `sweep`
				// CLI command, never as a genuinely long-running background
				// process. `serve` is the natural place for that, since
				// it's already a long-lived process.
				go func() {
					if err := r.Run(bgCtx); err != nil && !errors.Is(err, context.Canceled) {
						fmt.Fprintf(os.Stderr, "reaper stopped: %v\n", err)
					}
				}()
			}

			if !secureCookies {
				fmt.Fprintln(os.Stderr, "warning: --secure-cookies is false — session cookies will be sent over plain HTTP. Set --secure-cookies=true once this is served behind HTTPS (TR-05).")
			}

			server := api.NewServer(w.orch, w.store, r, authService, serviceAuthService, effectiveUpgradeStore, upgrade.StaticConnectionProvider{}, idempotencyStore, secureCookies, w.pool, w.connInfo)
			httpServer := &http.Server{Addr: addr, Handler: server}

			fmt.Printf("pgarchimigrator %s\n", version.Version)
			fmt.Printf("Listening on %s (dashboard: http://localhost%s/)\n", addr, addr)

			serveErrCh := make(chan error, 1)
			go func() { serveErrCh <- httpServer.ListenAndServe() }()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

			select {
			case err := <-serveErrCh:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					return fmt.Errorf("server error: %w", err)
				}
				return nil
			case <-sigCh:
				fmt.Println("\nShutting down...")
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer shutdownCancel()
				if err := httpServer.Shutdown(shutdownCtx); err != nil {
					return fmt.Errorf("graceful shutdown failed: %w", err)
				}
				return nil
			}
		},
	}

	cmd.Flags().StringVar(&stateDBPath, "state-db", config.Default().StateDBPath, "path to the SQLite state database")
	cmd.Flags().StringVar(&authDBPath, "auth-db", config.Default().AuthDBPath, "path to the SQLite auth database (users, sessions)")
	cmd.Flags().StringVar(&upgradeDBPath, "upgrade-db", config.Default().UpgradeDBPath, "path to the SQLite upgrade-progress database")
	cmd.Flags().StringVar(&addr, "addr", ":8080", "address to listen on")
	cmd.Flags().BoolVar(&autoSweep, "auto-sweep", true, "run engines/postgresql/reaper's periodic sweep loop in the background while serving")
	cmd.Flags().BoolVar(&secureCookies, "secure-cookies", false, "mark the session cookie Secure (set true once served behind HTTPS)")
	return cmd
}

// newAuthCmd groups authentication-related subcommands under `pgarchimigrator auth`.
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication (bootstrap the first admin user)",
	}
	cmd.AddCommand(newCreateAdminCmd())
	return cmd
}

// newCreateAdminCmd bootstraps the very first admin user for a fresh
// deployment. This is the ONLY way to create an account before any user
// exists — every subsequent user is created through the (admin-only)
// POST /api/users endpoint instead, once someone can log in to call it.
func newCreateAdminCmd() *cobra.Command {
	var (
		authDBPath string
		email      string
		password   string
		orgName    string
	)

	cmd := &cobra.Command{
		Use:   "create-admin",
		Short: "Bootstrap the first admin user for this deployment (run once)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if email == "" || password == "" {
				return fmt.Errorf("--email and --password are required")
			}
			if len(password) < 8 {
				return fmt.Errorf("password must be at least 8 characters")
			}

			store, err := auth.NewSQLiteStore(authDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the auth database (%s): %w", authDBPath, err)
			}
			defer store.Close()

			org, err := auth.EnsureDefaultOrganization(cmd.Context(), store, orgName)
			if err != nil {
				return fmt.Errorf("failed to set up the default organization: %w", err)
			}

			service := auth.NewService(store)
			newAdmin, err := service.CreateUser(cmd.Context(), org.ID, email, password, auth.RoleAdmin)
			if err != nil {
				if errors.Is(err, auth.ErrDuplicateEmail) {
					return fmt.Errorf("a user with email %q already exists — this deployment may already be bootstrapped", email)
				}
				return fmt.Errorf("failed to create admin user: %w", err)
			}

			fmt.Printf("Admin user created: %s (organization: %s)\n", newAdmin.Email, org.Name)
			fmt.Println("You can now log in at the dashboard, or via POST /api/auth/login.")
			return nil
		},
	}

	cmd.Flags().StringVar(&authDBPath, "auth-db", config.Default().AuthDBPath, "path to the SQLite auth database")
	cmd.Flags().StringVar(&email, "email", "", "admin email (required)")
	cmd.Flags().StringVar(&password, "password", "", "admin password, at least 8 characters (required)")
	cmd.Flags().StringVar(&orgName, "org", "Default Organization", "organization display name (only used on first bootstrap)")
	return cmd
}

// newEcosystemCmd groups commands for the Archi ecosystem integration
// layer (see docs/ecosystem/ARCHITECTURE.md) — today, just registering
// the service clients (e.g. one ArchiConsole deployment) allowed to
// call this product's API via OAuth2 client-credentials.
func newEcosystemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ecosystem",
		Short: "Manage Archi ecosystem integration (service clients for OAuth2 client-credentials auth)",
	}
	cmd.AddCommand(newCreateClientCmd())
	cmd.AddCommand(newListClientsCmd())
	return cmd
}

// newCreateClientCmd registers a new service client. Unlike
// `auth create-admin`'s --password (a value the operator chooses),
// the client secret here is always GENERATED — see
// serviceauth.GenerateClientSecret's own doc comment for why a
// machine-to-machine credential should be high-entropy and
// machine-generated, never human-chosen — and is printed to stdout
// exactly once, since serviceauth.Client only ever persists its hash
// (Client.ClientSecretHash's own doc comment: "shown ... exactly once
// ... never persisted or retrievable again"). If it's lost, the only
// recovery is deleting and re-creating the client.
func newCreateClientCmd() *cobra.Command {
	var (
		authDBPath string
		name       string
		scopesRaw  string
	)

	cmd := &cobra.Command{
		Use:   "create-client",
		Short: "Register a new service client (e.g. an ArchiConsole deployment) for OAuth2 client-credentials auth",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			var scopes []string
			for _, s := range strings.Split(scopesRaw, ",") {
				if s = strings.TrimSpace(s); s != "" {
					scopes = append(scopes, s)
				}
			}
			if len(scopes) == 0 {
				return fmt.Errorf("--scopes is required (comma-separated, e.g. pgarchimigrator.read,pgarchimigrator.migrate)")
			}

			store, err := serviceauth.NewSQLiteStore(authDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the service-auth database (%s): %w", authDBPath, err)
			}
			defer store.Close()

			rawSecret, secretHash, err := serviceauth.GenerateClientSecret()
			if err != nil {
				return fmt.Errorf("failed to generate a client secret: %w", err)
			}

			clientID := "client_" + randomHex(8)
			client := &serviceauth.Client{
				Name: name, ClientID: clientID, ClientSecretHash: secretHash, Scopes: scopes,
			}
			if err := store.CreateClient(cmd.Context(), client); err != nil {
				if errors.Is(err, serviceauth.ErrDuplicateClientID) {
					return fmt.Errorf("a client with this client_id already exists (this should be extremely rare — try again)")
				}
				return fmt.Errorf("failed to create client: %w", err)
			}

			fmt.Printf("Client registered: %s\n", client.Name)
			fmt.Printf("  client_id:     %s\n", client.ClientID)
			fmt.Printf("  client_secret: %s\n", rawSecret)
			fmt.Printf("  scopes:        %s\n", strings.Join(client.Scopes, ", "))
			fmt.Println()
			fmt.Println("Save the client_secret now — it is shown only this once and cannot be retrieved again.")
			return nil
		},
	}

	cmd.Flags().StringVar(&authDBPath, "auth-db", config.Default().AuthDBPath, "path to the SQLite auth database")
	cmd.Flags().StringVar(&name, "name", "", "a human-readable name for this client, e.g. \"ArchiConsole (production)\" (required)")
	cmd.Flags().StringVar(&scopesRaw, "scopes", "", "comma-separated scopes this client may request, e.g. pgarchimigrator.read,pgarchimigrator.migrate (required)")
	return cmd
}

func newListClientsCmd() *cobra.Command {
	var authDBPath string

	cmd := &cobra.Command{
		Use:   "list-clients",
		Short: "List registered service clients",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := serviceauth.NewSQLiteStore(authDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the service-auth database (%s): %w", authDBPath, err)
			}
			defer store.Close()

			clients, err := store.ListClients(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to list clients: %w", err)
			}
			if len(clients) == 0 {
				fmt.Println("No service clients registered.")
				return nil
			}
			for _, c := range clients {
				fmt.Printf("%-24s %-30s %s\n", c.ClientID, c.Name, strings.Join(c.Scopes, ","))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&authDBPath, "auth-db", config.Default().AuthDBPath, "path to the SQLite auth database")
	return cmd
}

// randomHex returns n random bytes, hex-encoded — used for a client_id
// (a PUBLIC identifier, unlike the client secret, so it doesn't need
// serviceauth.GenerateClientSecret's full 256 bits; this is deliberately
// smaller, matching how job IDs and other non-secret identifiers
// elsewhere in this project are sized).
func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// newUpgradeCmd groups commands for engines/postgresql/upgrade — PostgreSQL
// major-version upgrades (see docs/ecosystem/ARCHITECTURE.md's own
// "PostgreSQL major-version upgrade" section for the full design).
// Deliberately its own top-level command, not folded under an existing
// one — an upgrade job is conceptually unrelated to a single-table
// migration job (see engines/postgresql/upgrade's own package doc comment for why
// it's a parallel package rather than a new operation type), so it gets
// its own parallel command tree rather than living under `migrate` or
// sharing flags/state with it.
func newUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Sync an entire database from an old PostgreSQL instance to a new one (major-version upgrade)",
	}
	cmd.AddCommand(newUpgradeStartCmd())
	cmd.AddCommand(newUpgradeStatusCmd())
	cmd.AddCommand(newUpgradeListCmd())
	return cmd
}

func newUpgradeStartCmd() *cobra.Command {
	var (
		upgradeDBPath        string
		sourceDSN            string
		targetDSN            string
		sourceReplicationDSN string
		schemasRaw           string
		tablesRaw            string
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a database upgrade — introspects, syncs, and validates every in-scope table, then stops (no cutover)",
		Long: "Runs synchronously in the foreground, like apply-file — for a genuinely long-running\n" +
			"upgrade, run this under your own process supervisor (systemd, nohup, tmux) rather\n" +
			"than expecting this command itself to detach. Use `upgrade status` from another\n" +
			"terminal to follow progress while this runs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if sourceDSN == "" || targetDSN == "" {
				return fmt.Errorf("--source-dsn and --target-dsn are both required")
			}

			var schemas []string
			for _, s := range strings.Split(schemasRaw, ",") {
				if s = strings.TrimSpace(s); s != "" {
					schemas = append(schemas, s)
				}
			}

			// --tables takes precedence over --schemas — see
			// upgrade.Job.Tables' own doc comment for why the two are
			// mutually exclusive (table-level scoping vs whole-schema
			// scoping), matching the dashboard's own checkbox picker
			// sending exactly one or the other, never both.
			var tables []upgrade.TableRef
			for _, t := range strings.Split(tablesRaw, ",") {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				schema, table, found := strings.Cut(t, ".")
				if !found {
					return fmt.Errorf("--tables entries must be schema.table (got %q)", t)
				}
				tables = append(tables, upgrade.TableRef{SchemaName: schema, TableName: table})
			}

			store, err := upgrade.NewSQLiteStore(upgradeDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the upgrade database (%s): %w", upgradeDBPath, err)
			}
			defer store.Close()

			job := &upgrade.Job{
				Schemas:              schemas,
				SourceConnectionRef:  sourceDSN,
				TargetConnectionRef:  targetDSN,
				SourceReplicationRef: sourceReplicationDSN,
				Tables:               tables,
			}
			if err := store.CreateJob(cmd.Context(), job); err != nil {
				return fmt.Errorf("failed to create upgrade job: %w", err)
			}
			fmt.Printf("Upgrade job created: %s\n", job.ID)
			fmt.Println("Introspecting source, creating target schema, syncing, and validating — this can take a long time for a large database.")
			fmt.Printf("Run `pgarchimigrator upgrade status %s` from another terminal to follow progress.\n\n", job.ID)

			flow := &upgrade.Flow{Store: store, ConnectionProvider: upgrade.StaticConnectionProvider{}}
			if err := flow.Run(cmd.Context(), job); err != nil {
				return fmt.Errorf("upgrade failed (phase: %s): %w", job.Phase, err)
			}

			fmt.Printf("\nUpgrade ready: %d/%d tables synced and verified.\n", job.TablesVerified, job.TablesTotal)
			fmt.Println("This tool does not perform cutover — repointing application traffic at the new instance is a separate, deliberate step.")
			return nil
		},
	}

	cmd.Flags().StringVar(&upgradeDBPath, "upgrade-db", config.Default().UpgradeDBPath, "path to the SQLite upgrade-progress database")
	cmd.Flags().StringVar(&sourceDSN, "source-dsn", "", "connection string for the OLD-version source instance (required)")
	cmd.Flags().StringVar(&targetDSN, "target-dsn", "", "connection string for the NEW-version target instance, already running and reachable (required)")
	cmd.Flags().StringVar(&sourceReplicationDSN, "source-replication-dsn", "",
		"connection string the TARGET instance's own PostgreSQL server should use to reach the source for replication — only needed when it differs "+
			"from --source-dsn (e.g. this command reaches source via a host-mapped Docker port like localhost:55432, but the target container must "+
			"reach it via the Docker network's own hostname like pg-logical:5432). Defaults to --source-dsn if not set.")
	cmd.Flags().StringVar(&schemasRaw, "schemas", "", "comma-separated schemas to include (default: every schema on the source instance)")
	cmd.Flags().StringVar(&tablesRaw, "tables", "", "comma-separated schema.table pairs for table-level scoping — takes precedence over --schemas when set")
	return cmd
}

func newUpgradeStatusCmd() *cobra.Command {
	var upgradeDBPath string

	cmd := &cobra.Command{
		Use:   "status <job-id>",
		Short: "Show an upgrade job's current phase and per-table progress",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := upgrade.NewSQLiteStore(upgradeDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the upgrade database (%s): %w", upgradeDBPath, err)
			}
			defer store.Close()

			job, err := store.GetJob(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to look up upgrade job %s: %w", args[0], err)
			}
			fmt.Printf("Job %s — phase: %s\n", job.ID, job.Phase)
			if job.LastError != "" {
				fmt.Printf("  last error: %s\n", job.LastError)
			}
			fmt.Printf("  tables: %d total, %d synced, %d verified\n", job.TablesTotal, job.TablesSynced, job.TablesVerified)

			tables, err := store.ListTables(cmd.Context(), job.ID)
			if err != nil {
				return fmt.Errorf("failed to list tables for job %s: %w", job.ID, err)
			}
			for _, t := range tables {
				line := fmt.Sprintf("  %s.%s: %s", t.SchemaName, t.TableName, t.Phase)
				if t.LastError != "" {
					line += " (" + t.LastError + ")"
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&upgradeDBPath, "upgrade-db", config.Default().UpgradeDBPath, "path to the SQLite upgrade-progress database")
	return cmd
}

func newUpgradeListCmd() *cobra.Command {
	var upgradeDBPath string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List upgrade jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := upgrade.NewSQLiteStore(upgradeDBPath)
			if err != nil {
				return fmt.Errorf("failed to open the upgrade database (%s): %w", upgradeDBPath, err)
			}
			defer store.Close()

			jobs, err := store.ListJobs(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to list upgrade jobs: %w", err)
			}
			if len(jobs) == 0 {
				fmt.Println("No upgrade jobs found.")
				return nil
			}
			for _, j := range jobs {
				fmt.Printf("%-28s %-16s %d/%d tables verified\n", j.ID, j.Phase, j.TablesVerified, j.TablesTotal)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&upgradeDBPath, "upgrade-db", config.Default().UpgradeDBPath, "path to the SQLite upgrade-progress database")
	return cmd
}
