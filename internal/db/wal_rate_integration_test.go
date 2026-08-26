//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/db/... -tags=integration -v -run WALGenerationRate
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
)

// TestSampleWALGenerationRate_DetectsRealWriteActivity is the direct
// end-to-end proof this works against real PostgreSQL: generates a
// deliberate burst of writes on a separate, concurrent connection DURING
// the sample window, and confirms the measured rate reflects it — a
// near-zero result here would mean the LSN-diff math itself is wrong,
// not just that nothing happened to be writing during this particular
// test run.
func TestSampleWALGenerationRate_DetectsRealWriteActivity(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()
	tableName := "wal_rate_test_table"

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tableName)
	if _, err := pool.Exec(ctx, `CREATE TABLE `+tableName+` (id BIGINT, payload TEXT)`); err != nil {
		t.Fatalf("could not create test table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+tableName) })

	sampleDuration := 3 * time.Second
	stopWriting := make(chan struct{})
	go func() {
		// Continuous inserts with a reasonably large payload — enough
		// real WAL volume to be unambiguously detectable, without this
		// test needing to wait as long as a real load test would.
		for {
			select {
			case <-stopWriting:
				return
			default:
				_, _ = pool.Exec(context.Background(),
					`INSERT INTO `+tableName+` (id, payload) SELECT g, repeat('x', 1000) FROM generate_series(1, 100) g`)
			}
		}
	}()
	t.Cleanup(func() { close(stopWriting) })

	rate, err := db.SampleWALGenerationRate(ctx, pool, sampleDuration)
	if err != nil {
		t.Fatalf("SampleWALGenerationRate failed: %v", err)
	}
	if rate <= 0 {
		t.Errorf("expected a positive WAL generation rate while writes are actively happening, got %v bytes/sec", rate)
	}
}

func TestSampleWALGenerationRate_ReturnsNonNegativeOnAQuietServer(t *testing.T) {
	pool := connect(t, logicalDSN)
	ctx := context.Background()

	rate, err := db.SampleWALGenerationRate(ctx, pool, 1*time.Second)
	if err != nil {
		t.Fatalf("SampleWALGenerationRate failed: %v", err)
	}
	if rate < 0 {
		t.Errorf("expected a non-negative rate, got %v", rate)
	}
}
