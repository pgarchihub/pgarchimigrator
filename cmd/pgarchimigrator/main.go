// Command pgarchimigrator is the CLI entry point for pgArchiMigrator
// (Architecture Doc Section 5: "CLI (Cobra) + REST API").
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/pgarchihub/pgarchimigrator/internal/api"
	"github.com/pgarchihub/pgarchimigrator/internal/auditlog"
	"github.com/pgarchihub/pgarchimigrator/internal/auth"
	"github.com/pgarchihub/pgarchimigrator/internal/config"
	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/ddlflow"
	"github.com/pgarchihub/pgarchimigrator/internal/migrationfile"
	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/preview"
	"github.com/pgarchihub/pgarchimigrator/internal/progress"
	"github.com/pgarchihub/pgarchimigrator/internal/reaper"
	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
	"github.com/pgarchihub/pgarchimigrator/internal/typecompat"
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

	flowFor := func(strat strategy.Strategy) (orchestrator.Flow, error) {
		switch strat {
		case strategy.StrategyDirectDDL, strategy.StrategyExpandBackfill:
			return ddlflow.New(pool, store), nil
		case strategy.StrategyShadowTable:
			return shadowflow.New(pool, replicationDSN, store, preflighter), nil
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

	orch := orchestrator.New(store, flowFor, tableStats)
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
		stateDBPath      string
		schemaName       string
		tableName        string
		columnName       string
		operationStr     string
		columnType       string
		defaultValue     string
		isVolatile       bool
		strategyOverride string
		indexName        string
		constraintName   string
		checkExpression  string
		newColumnName    string
		dryRun           bool
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Start a schema-change migration (FR-01..FR-04)",
		RunE: func(cmd *cobra.Command, args []string) error {
			op := strategy.Operation(operationStr)

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
			// change is one of internal/typecompat's curated "free" cases
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
	cmd.Flags().StringVar(&columnName, "column", "", "target column name (required for ADD_COLUMN, DROP_COLUMN, ALTER_COLUMN_TYPE, ADD_INDEX, SET_NOT_NULL, RENAME_COLUMN — the existing name for RENAME_COLUMN)")
	cmd.Flags().StringVar(&operationStr, "operation", "", "ADD_COLUMN, DROP_COLUMN, ALTER_COLUMN_TYPE, ADD_INDEX, DROP_INDEX, SET_NOT_NULL, ADD_CONSTRAINT, or RENAME_COLUMN (required)")
	cmd.Flags().StringVar(&columnType, "type", "", "new column type (required for ALTER_COLUMN_TYPE, or the type of the column being added)")
	cmd.Flags().StringVar(&defaultValue, "default", "", "default value expression for ADD_COLUMN (e.g. \"'active'\" or \"now()\")")
	cmd.Flags().BoolVar(&isVolatile, "volatile-default", false, "set if --default is a volatile expression (e.g. now()), triggering Expand & Backfill")
	cmd.Flags().StringVar(&strategyOverride, "strategy", "", "override the automatic strategy decision (DIRECT_DDL, EXPAND_BACKFILL, SHADOW_TABLE)")
	cmd.Flags().StringVar(&indexName, "index-name", "", "index name for ADD_INDEX (optional, auto-generated as idx_<table>_<column> if omitted) or DROP_INDEX (required)")
	cmd.Flags().StringVar(&constraintName, "constraint-name", "", "constraint name for SET_NOT_NULL (optional, auto-generated if omitted) or ADD_CONSTRAINT (required)")
	cmd.Flags().StringVar(&checkExpression, "check-expression", "", "CHECK(...) expression body for ADD_CONSTRAINT, e.g. \"price > 0\" (required)")
	cmd.Flags().StringVar(&newColumnName, "new-column-name", "", "the new name for RENAME_COLUMN (required) — see the command's long help for why this doesn't do a plain ALTER TABLE RENAME")
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

				req := m.ToMigrationRequest(currentActor())
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

				req := m.ToMigrationRequest(currentActor())
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
// automatically via reaper.Run's periodic loop (see internal/reaper); this
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

			server := api.NewServer(w.orch, w.store, r, authService, secureCookies, w.pool, w.connInfo)
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
	cmd.Flags().StringVar(&addr, "addr", ":8080", "address to listen on")
	cmd.Flags().BoolVar(&autoSweep, "auto-sweep", true, "run internal/reaper's periodic sweep loop in the background while serving")
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
