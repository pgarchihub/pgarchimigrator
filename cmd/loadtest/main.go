// Command loadtest is a standalone tool for load-testing
// pgArchiMigrator's zero-downtime claim against a large table — it does
// NOT import any internal/ package (deliberately: it talks to the REST
// API exactly like a real external client would, the same way a real
// production monitoring/synthetic-traffic tool would), and it never
// touches the pgArchiMigrator server's own state; it only generates data
// in and traffic against the TARGET database being migrated.
//
// Usage:
//
//	go run ./cmd/loadtest generate --dsn "postgresql://..." --rows 10000000
//	go run ./cmd/loadtest run --dsn "postgresql://..." --api-url "http://localhost:8080" \
//	    --admin-email admin@example.com --admin-password ... \
//	    --operation ADD_COLUMN --column loadtest_flag --column-type boolean --default false
//
// To specifically exercise EXPAND_BACKFILL's batched-write strategy
// (rather than DIRECT_DDL's metadata-only fast path), the default must
// be BOTH a genuinely volatile expression AND flagged as such — the
// server has no way to infer volatility from the string alone:
//
//	go run ./cmd/loadtest run --dsn "postgresql://..." --api-url "http://localhost:8080" \
//	    --admin-email admin@example.com --admin-password ... \
//	    --operation ADD_COLUMN --column created_ts --column-type timestamptz \
//	    --default "now()" --volatile-default
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "loadtest",
		Short: "Load-test pgArchiMigrator against a large (10M+ row) table",
	}
	root.AddCommand(newGenerateCmd())
	root.AddCommand(newRunCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// pgxIdent quotes a PostgreSQL identifier — the table name here comes
// from a command-line flag, not a hardcoded literal, so it's escaped the
// same way the rest of this project always escapes identifiers built
// from external input (see internal/ddlflow's quoteIdent and its several
// sibling copies — this is deliberately a 6th, since this tool is a
// separate binary with no internal/ import, matching its own
// external-REST-client design).
func pgxIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// --- generate: bulk-create a large test table ------------------------------

func newGenerateCmd() *cobra.Command {
	var dsn, tableName string
	var rows int64
	var batchSize int64

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Create and bulk-populate a load-test table with N rows",
		Long: "Creates a plausible \"orders\"-shaped table and fills it using PostgreSQL's own " +
			"generate_series() (a single bulk INSERT per batch) rather than inserting row-by-row " +
			"from this tool — the latter would take hours at 10M+ rows; the former takes minutes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, dsn)
			if err != nil {
				return fmt.Errorf("failed to connect to %s: %w", dsn, err)
			}
			defer pool.Close()

			quotedTable := pgxIdent(tableName)

			fmt.Printf("Creating table %s (if it doesn't already exist)...\n", tableName)
			createSQL := fmt.Sprintf(`
				CREATE TABLE IF NOT EXISTS %s (
					id BIGSERIAL PRIMARY KEY,
					customer_id BIGINT NOT NULL,
					amount NUMERIC(12,2) NOT NULL,
					status TEXT NOT NULL DEFAULT 'pending',
					created_at TIMESTAMPTZ NOT NULL DEFAULT now()
				)`, quotedTable)
			if _, err := pool.Exec(ctx, createSQL); err != nil {
				return fmt.Errorf("failed to create table: %w", err)
			}

			var existing int64
			if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", quotedTable)).Scan(&existing); err != nil {
				return fmt.Errorf("failed to count existing rows: %w", err)
			}
			if existing > 0 {
				fmt.Printf("Table already has %d rows — skipping generation. Drop the table first if you want a fresh %d-row set.\n", existing, rows)
				return nil
			}

			fmt.Printf("Generating %d rows in batches of %d (this may take a few minutes)...\n", rows, batchSize)
			start := time.Now()
			insertSQL := fmt.Sprintf(`
				INSERT INTO %s (customer_id, amount, status, created_at)
				SELECT
					(random() * 100000)::bigint,
					(random() * 10000)::numeric(12,2),
					(ARRAY['pending','completed','cancelled'])[floor(random() * 3 + 1)],
					now() - (random() * interval '365 days')
				FROM generate_series(1, $1)
			`, quotedTable)

			for inserted := int64(0); inserted < rows; inserted += batchSize {
				thisBatch := batchSize
				if inserted+batchSize > rows {
					thisBatch = rows - inserted
				}
				if _, err := pool.Exec(ctx, insertSQL, thisBatch); err != nil {
					return fmt.Errorf("failed to insert batch starting at row %d: %w", inserted, err)
				}
				fmt.Printf("  %d / %d rows inserted (%s elapsed)\n", inserted+thisBatch, rows, time.Since(start).Round(time.Second))
			}

			fmt.Printf("Row generation done in %s. Creating an index on customer_id (realistic read pattern)...\n", time.Since(start).Round(time.Second))
			idxSQL := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (customer_id)",
				pgxIdent(tableName+"_customer_id_idx"), quotedTable)
			if _, err := pool.Exec(ctx, idxSQL); err != nil {
				return fmt.Errorf("failed to create index: %w", err)
			}

			fmt.Println("Table ready. Run `loadtest run` to load-test a migration against it.")
			return nil
		},
	}

	cmd.Flags().StringVar(&dsn, "dsn", "", "target PostgreSQL DSN, e.g. postgresql://user:pass@host:5432/db (required)")
	cmd.Flags().StringVar(&tableName, "table", "loadtest_orders", "table name to create")
	cmd.Flags().Int64Var(&rows, "rows", 10_000_000, "number of rows to generate")
	cmd.Flags().Int64Var(&batchSize, "batch-size", 1_000_000, "rows inserted per batch (smaller batches use less memory but take a bit longer)")
	_ = cmd.MarkFlagRequired("dsn")
	return cmd
}

// --- run: concurrent traffic + a real migration, then a latency report -----

type latencySample struct {
	at              time.Time
	duringMigration bool
	duration        time.Duration
	err             error
}

func newRunCmd() *cobra.Command {
	var dsn, apiURL, adminEmail, adminPassword, tableName string
	var operation, column, columnType, defaultValue, indexName, newColumnName, checkExpression, strategyOverride string
	var volatileDefault bool
	var concurrency int
	var warmup, cooldown time.Duration

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run concurrent traffic against the load-test table while triggering a real migration",
		Long: "Starts `concurrency` goroutines issuing a realistic read/write mix directly against " +
			"the target table, triggers a real migration through the pgArchiMigrator REST API partway " +
			"through, polls until it completes, and reports query latency before/during/after — this " +
			"is the actual measurement of whether the migration lived up to \"zero-downtime\": query " +
			"latency during the migration should look statistically like latency before and after it, " +
			"not spike.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, dsn)
			if err != nil {
				return fmt.Errorf("failed to connect to target database: %w", err)
			}
			defer pool.Close()

			quotedTable := pgxIdent(tableName)
			var rowCount int64
			if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", quotedTable)).Scan(&rowCount); err != nil {
				return fmt.Errorf("failed to count rows in %s (did you run `generate` first?): %w", tableName, err)
			}
			fmt.Printf("Target table %s has %d rows.\n", tableName, rowCount)

			// --- traffic generator: a realistic OLTP-ish read/write mix,
			// running continuously in the background until the whole test
			// window (warmup + migration + cooldown) is over. ---
			// Buffered just enough to smooth out brief bursts between the
			// producers (traffic goroutines) and the single collector
			// goroutine below — not sized to hold a whole test run's worth
			// of samples, since continuous draining means it never needs
			// to be.
			samplesCh := make(chan latencySample, 10_000)
			stopTraffic := make(chan struct{})
			duringMigration := &atomicBool{}

			var wg sync.WaitGroup
			for i := 0; i < concurrency; i++ {
				wg.Add(1)
				go func(seed int64) {
					defer wg.Done()
					rng := rand.New(rand.NewSource(seed))
					for {
						select {
						case <-stopTraffic:
							return
						default:
						}
						start := time.Now()
						var qErr error
						if rng.Intn(5) == 0 {
							// ~20% writes: touch one row by primary key.
							id := rng.Int63n(rowCount) + 1
							_, qErr = pool.Exec(ctx, fmt.Sprintf("UPDATE %s SET amount = amount WHERE id = $1", quotedTable), id)
						} else {
							// ~80% reads: a customer_id lookup, the index built by `generate`.
							customerID := rng.Int63n(100_000)
							var count int
							qErr = pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE customer_id = $1", quotedTable), customerID).Scan(&count)
						}
						samplesCh <- latencySample{
							at:              start,
							duringMigration: duringMigration.get(),
							duration:        time.Since(start),
							err:             qErr,
						}
					}
				}(time.Now().UnixNano() + int64(i))
			}

			// samplesCh is drained CONTINUOUSLY by a dedicated collector
			// goroutine below, not just once at the end — with a fixed
			// buffer size and only draining after every traffic goroutine
			// stops, a long/high-throughput run could fill the buffer
			// while traffic is still active, deadlocking every traffic
			// goroutine on a blocked channel send with nothing left to
			// drain it. A single, continuously-running collector goroutine
			// avoids that regardless of test duration or throughput.
			var before, during []latencySample
			collectDone := make(chan struct{})
			go func() {
				defer close(collectDone)
				for s := range samplesCh {
					if s.duringMigration {
						during = append(during, s)
					} else {
						before = append(before, s)
					}
				}
			}()

			client := &apiClient{baseURL: apiURL}
			fmt.Println("Logging in...")
			if err := client.login(adminEmail, adminPassword); err != nil {
				close(stopTraffic)
				wg.Wait()
				return fmt.Errorf("login failed: %w", err)
			}

			fmt.Printf("Warming up (%s baseline before starting the migration)...\n", warmup)
			time.Sleep(warmup)

			fmt.Printf("Starting migration: %s on %s...\n", operation, tableName)
			migrationStart := time.Now()
			duringMigration.set(true)

			// Direct evidence, not guesswork: periodically checks
			// pg_locks/pg_stat_activity for genuine lock waits during the
			// migration — see monitorLocks' doc comment for why this
			// exists. Stopped via lockMonitorDone once the migration
			// finishes.
			lockMonitorStop := make(chan struct{})
			lockMonitorDone := make(chan struct{})
			var lockObservations []lockObservation
			go func() {
				defer close(lockMonitorDone)
				lockObservations = monitorLocks(ctx, pool, migrationStart, lockMonitorStop)
			}()

			jobID, err := client.startMigration(startMigrationRequest{
				Table: tableName, Operation: operation, Column: column, Type: columnType,
				Default: defaultValue, VolatileDefault: volatileDefault, IndexName: indexName,
				NewColumnName: newColumnName, CheckExpression: checkExpression, StrategyOverride: strategyOverride,
			})
			if err != nil {
				duringMigration.set(false)
				close(lockMonitorStop)
				close(stopTraffic)
				wg.Wait()
				return fmt.Errorf("failed to start migration: %w", err)
			}
			fmt.Printf("Migration job: %s\n", jobID)

			var strategy string
			var migrationFailed bool
			var lastError string
			for {
				report, err := client.getMigration(jobID)
				if err != nil {
					fmt.Printf("  (poll error, retrying: %v)\n", err)
					time.Sleep(3 * time.Second)
					continue
				}
				strategy = report.Strategy
				fmt.Printf("  phase=%-16s percent=%3.0f%%\n", report.CurrentPhase, report.PercentComplete)
				if report.Terminal {
					migrationFailed = report.Failed
					lastError = report.LastError
					break
				}
				time.Sleep(3 * time.Second)
			}
			duringMigration.set(false)
			close(lockMonitorStop)
			<-lockMonitorDone

			if migrationFailed {
				fmt.Printf("Migration FAILED: %s\n", lastError)
			} else {
				fmt.Println("Migration completed.")
			}

			fmt.Printf("Cooling down (%s baseline after the migration)...\n", cooldown)
			time.Sleep(cooldown)

			close(stopTraffic)
			wg.Wait()
			close(samplesCh)
			<-collectDone // wait for the collector goroutine to finish draining everything sent before the close

			printReport(strategy, migrationStart, before, during, lockObservations)
			return nil
		},
	}

	cmd.Flags().StringVar(&dsn, "dsn", "", "target PostgreSQL DSN (required)")
	cmd.Flags().StringVar(&apiURL, "api-url", "http://localhost:8080", "pgArchiMigrator server base URL")
	cmd.Flags().StringVar(&adminEmail, "admin-email", "", "an operator/admin account email (required)")
	cmd.Flags().StringVar(&adminPassword, "admin-password", "", "that account's password (required)")
	cmd.Flags().StringVar(&tableName, "table", "loadtest_orders", "table to migrate (must already exist — run `generate` first)")
	cmd.Flags().StringVar(&operation, "operation", "ADD_COLUMN", "operation to test: ADD_COLUMN, DROP_COLUMN, ALTER_COLUMN_TYPE, ADD_INDEX, DROP_INDEX, SET_NOT_NULL, ADD_CONSTRAINT, RENAME_COLUMN")
	cmd.Flags().StringVar(&column, "column", "loadtest_flag", "column name (ADD_COLUMN/DROP_COLUMN/ALTER_COLUMN_TYPE/SET_NOT_NULL/RENAME_COLUMN)")
	cmd.Flags().StringVar(&columnType, "column-type", "boolean", "column type (ADD_COLUMN/ALTER_COLUMN_TYPE)")
	cmd.Flags().StringVar(&defaultValue, "default", "false", "default value (ADD_COLUMN)")
	cmd.Flags().BoolVar(&volatileDefault, "volatile-default", false,
		"set true alongside --default when the default is volatile (e.g. now(), random()) — this is what actually "+
			"triggers EXPAND_BACKFILL's batched-write strategy instead of DIRECT_DDL's metadata-only fast path; "+
			"the --default STRING VALUE alone (e.g. \"now()\") does NOT do this on its own, the server only looks "+
			"at this separate flag to decide")
	cmd.Flags().StringVar(&indexName, "index-name", "", "index name (ADD_INDEX/DROP_INDEX) — auto-generated if empty")
	cmd.Flags().StringVar(&newColumnName, "new-column-name", "", "new column name (RENAME_COLUMN)")
	cmd.Flags().StringVar(&checkExpression, "check-expression", "", "CHECK expression (ADD_CONSTRAINT)")
	cmd.Flags().StringVar(&strategyOverride, "strategy-override", "", "force a specific strategy: DIRECT_DDL, EXPAND_BACKFILL, or SHADOW_TABLE — "+
		"needed to reliably load-test SHADOW_TABLE via ALTER_COLUMN_TYPE, since the server otherwise picks automatically based on "+
		"whether the old/new types are compatible (see internal/typecompat), which isn't always obvious from the column types alone")
	cmd.Flags().IntVar(&concurrency, "concurrency", 20, "number of concurrent traffic goroutines")
	cmd.Flags().DurationVar(&warmup, "warmup", 30*time.Second, "baseline traffic duration before starting the migration")
	cmd.Flags().DurationVar(&cooldown, "cooldown", 30*time.Second, "baseline traffic duration after the migration completes")
	_ = cmd.MarkFlagRequired("dsn")
	_ = cmd.MarkFlagRequired("admin-email")
	_ = cmd.MarkFlagRequired("admin-password")
	return cmd
}

// --- lock diagnostics ----------------------------------------------------

type lockObservation struct {
	at               time.Time
	blockedPID       int32
	blockingPID      int32
	blockingState    string
	blockingDuration time.Duration
	blockingQuery    string
}

// blockingLocksQuery is PostgreSQL's own canonical "who's blocking whom"
// query (this exact join shape is the one documented on the PostgreSQL
// wiki's lock-monitoring page) — it only returns a row when one backend
// is GENUINELY waiting to acquire a lock currently held by another.
const blockingLocksQuery = `
	SELECT
		blocked_locks.pid,
		blocking_locks.pid,
		blocking_activity.state,
		EXTRACT(EPOCH FROM (now() - blocking_activity.query_start)),
		blocking_activity.query
	FROM pg_catalog.pg_locks blocked_locks
	JOIN pg_catalog.pg_stat_activity blocked_activity ON blocked_activity.pid = blocked_locks.pid
	JOIN pg_catalog.pg_locks blocking_locks
		ON blocking_locks.locktype = blocked_locks.locktype
		AND blocking_locks.database IS NOT DISTINCT FROM blocked_locks.database
		AND blocking_locks.relation IS NOT DISTINCT FROM blocked_locks.relation
		AND blocking_locks.page IS NOT DISTINCT FROM blocked_locks.page
		AND blocking_locks.tuple IS NOT DISTINCT FROM blocked_locks.tuple
		AND blocking_locks.virtualxid IS NOT DISTINCT FROM blocked_locks.virtualxid
		AND blocking_locks.transactionid IS NOT DISTINCT FROM blocked_locks.transactionid
		AND blocking_locks.classid IS NOT DISTINCT FROM blocked_locks.classid
		AND blocking_locks.objid IS NOT DISTINCT FROM blocked_locks.objid
		AND blocking_locks.objsubid IS NOT DISTINCT FROM blocked_locks.objsubid
		AND blocking_locks.pid != blocked_locks.pid
	JOIN pg_catalog.pg_stat_activity blocking_activity ON blocking_activity.pid = blocking_locks.pid
	WHERE NOT blocked_locks.granted
`

// monitorLocks exists to turn a guess into evidence. A latency outlier in
// the traffic samples alone can't distinguish between two very different
// root causes: (a) a genuine PostgreSQL lock wait (one backend blocked on
// a lock another backend holds — an APPLICATION-level fix, like the
// lock_timeout/cursor-batching work already done in internal/ddlflow), or
// (b) the query was simply slow to EXECUTE — no lock involved at all,
// e.g. checkpoint I/O pressure, WAL fsync contention, or the test
// environment's hardware being genuinely saturated by the combined write
// load of concurrent traffic plus a full-speed backfill. These need
// completely different fixes, and only the database itself can say which
// one actually happened — pg_locks either shows a real blocked/blocking
// pair, or it doesn't, and that's the whole answer. Polled every 3s
// rather than continuously to keep this diagnostic itself cheap relative
// to the real traffic being measured.
func monitorLocks(ctx context.Context, pool *pgxpool.Pool, migrationStart time.Time, stop <-chan struct{}) []lockObservation {
	var observations []lockObservation
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return observations
		case <-ticker.C:
			rows, err := pool.Query(ctx, blockingLocksQuery)
			if err != nil {
				continue // best-effort diagnostic — never fail the actual load test over this
			}
			for rows.Next() {
				var o lockObservation
				var blockingSeconds float64
				if err := rows.Scan(&o.blockedPID, &o.blockingPID, &o.blockingState, &blockingSeconds, &o.blockingQuery); err != nil {
					continue
				}
				o.blockingDuration = time.Duration(blockingSeconds * float64(time.Second))
				o.at = time.Now()
				observations = append(observations, o)
				fmt.Printf("  [LOCK WAIT +%s] pid %d blocked by pid %d (that backend has been %s for %s) — blocking query: %.120s\n",
					o.at.Sub(migrationStart).Round(time.Second), o.blockedPID, o.blockingPID, o.blockingState, o.blockingDuration.Round(time.Second), o.blockingQuery)
			}
			rows.Close()
		}
	}
}

// --- latency reporting -------------------------------------------------

func printReport(strategy string, migrationStart time.Time, before, during []latencySample, lockObservations []lockObservation) {
	fmt.Println()
	fmt.Println("=== Load Test Report ===")
	fmt.Printf("Strategy used: %s\n", strategy)
	fmt.Println()
	printLatencyStats("BEFORE/AFTER migration (baseline)", before)
	printLatencyStats("DURING migration", during)
	fmt.Println()

	beforeP99 := percentile(durations(before), 99)
	duringP99 := percentile(durations(during), 99)
	if beforeP99 > 0 {
		ratio := float64(duringP99) / float64(beforeP99)
		fmt.Printf("p99 latency during the migration was %.1fx the baseline p99.\n", ratio)
		switch {
		case ratio < 1.5:
			fmt.Println("=> Looks like a genuinely low-impact migration.")
		case ratio < 3:
			fmt.Println("=> Some measurable impact during the migration — worth a closer look at which phase caused it.")
		default:
			fmt.Println("=> Significant latency degradation during the migration — investigate before relying on this operation/table-size combination in production.")
		}
	}

	// This is the actual diagnosis, not a guess: if pg_locks ever showed a
	// real blocked/blocking pair, ANY latency outliers above are a
	// genuine PostgreSQL-level lock wait — an application fix belongs in
	// internal/ddlflow (or wherever the blocking query points to). If
	// this stayed empty despite outliers being reported below, the
	// outliers were NOT caused by a lock at all — the query was simply
	// slow to execute, which points at the test environment's own
	// resource headroom (disk I/O, checkpoint pressure, CPU) rather than
	// application code.
	if len(lockObservations) == 0 {
		fmt.Println("\nNo PostgreSQL lock wait was ever observed during the migration (checked every 3s) — any latency outliers below were NOT caused by lock contention.")
	} else {
		fmt.Printf("\n%d lock wait observation(s) during the migration — see the [LOCK WAIT ...] lines printed live above for the blocking query text.\n", len(lockObservations))
	}

	printOutliers(migrationStart, before, during)
}

// printOutliers exists because percentiles alone can hide a real, rare
// spike: a p99 that looks perfectly healthy in aggregate can still
// coexist with a single query that took multiple seconds — statistically
// invisible at the 99th percentile out of a million+ samples, but a very
// real bad experience for whoever's request that was. The threshold is
// the worst latency actually observed at baseline: anything during the
// migration slower than the worst thing already considered "normal
// enough" is worth a second look. Each outlier's time-since-migration-start
// is printed specifically so it can be lined up against the "phase=...
// percent=...%" progress lines already printed while the migration ran —
// e.g. "this spike happened right around the SYNCING→VALIDATING
// transition" is a genuinely actionable clue; "the p99 was fine" is not.
func printOutliers(migrationStart time.Time, before, during []latencySample) {
	threshold := percentile(durations(before), 100) // the worst latency baseline itself considered normal
	if threshold <= 0 {
		threshold = 100 * time.Millisecond // no baseline samples to compare against — fall back to a fixed bar
	}

	var outliers []latencySample
	for _, s := range during {
		if s.err == nil && s.duration > threshold {
			outliers = append(outliers, s)
		}
	}
	if len(outliers) == 0 {
		fmt.Printf("\nNo query during the migration was slower than the worst baseline query (%s) — no outliers to report.\n", threshold.Round(time.Millisecond))
		return
	}

	sort.Slice(outliers, func(i, j int) bool { return outliers[i].duration > outliers[j].duration })
	const maxShown = 20
	fmt.Printf("\n%d quer%s during the migration exceeded the worst baseline latency (%s) — the top %d, slowest first:\n",
		len(outliers), pluralY(len(outliers)), threshold.Round(time.Millisecond), min(maxShown, len(outliers)))
	for i, o := range outliers {
		if i >= maxShown {
			fmt.Printf("  ... and %d more\n", len(outliers)-maxShown)
			break
		}
		fmt.Printf("  +%-10s into the migration: %s\n", o.at.Sub(migrationStart).Round(time.Millisecond), o.duration.Round(time.Millisecond))
	}
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func printLatencyStats(label string, samples []latencySample) {
	d := durations(samples)
	errCount := 0
	for _, s := range samples {
		if s.err != nil {
			errCount++
		}
	}
	fmt.Printf("%s (%d queries, %d errors):\n", label, len(samples), errCount)
	if len(d) == 0 {
		fmt.Println("  (no samples)")
		return
	}
	fmt.Printf("  p50=%s  p95=%s  p99=%s  max=%s\n",
		percentile(d, 50).Round(time.Millisecond),
		percentile(d, 95).Round(time.Millisecond),
		percentile(d, 99).Round(time.Millisecond),
		d[len(d)-1].Round(time.Millisecond),
	)
}

func durations(samples []latencySample) []time.Duration {
	d := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		if s.err == nil {
			d = append(d, s.duration)
		}
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted) * p) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// atomicBool is a tiny helper — sync/atomic.Bool would work too, but this
// keeps the dependency surface (and the mental overhead of a generics-era
// API for something this small) minimal for a standalone test tool.
type atomicBool struct {
	mu sync.RWMutex
	v  bool
}

func (a *atomicBool) set(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
}

func (a *atomicBool) get() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.v
}

// --- minimal REST client (deliberately independent of internal/api's own
// client-side types — this tool talks to the server exactly like any
// external client would, over plain HTTP, with no privileged access to
// the server's own Go types). ---

type apiClient struct {
	baseURL string
	jar     *cookiejar.Jar
}

func (c *apiClient) httpClient(timeout time.Duration) (*http.Client, error) {
	if c.jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
		c.jar = jar
	}
	return &http.Client{Jar: c.jar, Timeout: timeout}, nil
}

// startMigrationTimeout is deliberately generous, unlike the short
// timeout used for login/getMigration — see the doc comment on
// (*apiClient).startMigration for why: unlike a typical "kick off a
// background job, return immediately" API, POST /api/migrations here
// blocks for the ENTIRE migration, not just until the job is created —
// a real finding this tool itself surfaced (see cmd/loadtest's own
// package doc comment for the "why" this matters beyond just this
// tool's own timeout setting).
const startMigrationTimeout = 30 * time.Minute
const shortAPITimeout = 30 * time.Second

func (c *apiClient) login(email, password string) error {
	hc, err := c.httpClient(shortAPITimeout)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := hc.Post(c.baseURL+"/api/auth/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiErrorFromResponse(resp)
	}
	return nil
}

type startMigrationRequest struct {
	Table            string `json:"table"`
	Operation        string `json:"operation"`
	Column           string `json:"column,omitempty"`
	Type             string `json:"type,omitempty"`
	Default          string `json:"default,omitempty"`
	VolatileDefault  bool   `json:"volatile_default"`
	IndexName        string `json:"index_name,omitempty"`
	NewColumnName    string `json:"new_column_name,omitempty"`
	CheckExpression  string `json:"check_expression,omitempty"`
	StrategyOverride string `json:"strategy_override,omitempty"`
}

// startMigration blocks until the migration is FULLY COMPLETE, not just
// until the job is created — this server's POST /api/migrations calls
// flow.Execute(...) synchronously inline (see
// internal/orchestrator.Orchestrator.StartMigration), unlike a typical
// "kick off a background job, return a job ID immediately, poll for
// status" REST design. For a fast DIRECT_DDL operation this is
// unnoticeable; for EXPAND_BACKFILL against a 10M+ row table with a
// volatile default, this single HTTP request can legitimately take
// several minutes — hence startMigrationTimeout being generous (30m)
// rather than the short timeout used for login/getMigration. In a real
// production deployment behind a reverse proxy, this same behavior is
// worth being aware of: a proxy's own default request timeout (often
// 30-60s) could sever the connection well before a large EXPAND_BACKFILL
// finishes, even though the migration itself keeps running server-side
// unaffected — only the HTTP response confirming it never arrives.
func (c *apiClient) startMigration(req startMigrationRequest) (string, error) {
	hc, err := c.httpClient(startMigrationTimeout)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(req)
	resp, err := hc.Post(c.baseURL+"/api/migrations", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 422 is a special case, deliberately checked BEFORE the generic
	// apiErrorFromResponse fallback below: unlike every other non-2xx
	// status this client sees (a plain {"error": "..."} body), a 422
	// here means the job WAS created and Execute() ran, but the
	// migration itself failed partway through (see internal/api's
	// handleStartMigration doc comment on this exact status choice) —
	// the response body is a full migration report (JobID + LastError),
	// not an {"error": ...} shape. Falling through to
	// apiErrorFromResponse would silently find no "error" field and
	// report a useless generic "status 422" message, hiding the actual
	// reason the migration failed — a real bug this tool itself hit
	// once, on exactly this status code.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		var report migrationReport
		if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
			return "", fmt.Errorf("migration failed (HTTP 422), and its response body could not be decoded either: %w", err)
		}
		return "", fmt.Errorf("migration failed: %s", report.LastError)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", apiErrorFromResponse(resp)
	}
	var result struct {
		JobID string `json:"JobID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode start-migration response: %w", err)
	}
	return result.JobID, nil
}

type migrationReport struct {
	CurrentPhase    string  `json:"CurrentPhase"`
	Strategy        string  `json:"Strategy"`
	PercentComplete float64 `json:"PercentComplete"`
	Terminal        bool    `json:"Terminal"`
	Failed          bool    `json:"Failed"`
	LastError       string  `json:"LastError"`
}

func (c *apiClient) getMigration(jobID string) (*migrationReport, error) {
	hc, err := c.httpClient(shortAPITimeout)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Get(c.baseURL + "/api/migrations/" + jobID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiErrorFromResponse(resp)
	}
	var report migrationReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return nil, fmt.Errorf("failed to decode migration report: %w", err)
	}
	return &report, nil
}

func apiErrorFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return fmt.Errorf("%s (status %d)", parsed.Error, resp.StatusCode)
	}
	return fmt.Errorf("request failed with status %d", resp.StatusCode)
}
