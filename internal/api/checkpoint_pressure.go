package api

import (
	"context"
	"log"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/progress"
)

// attachCheckpointPressure enriches report in place with a live
// checkpoint-pressure check — a no-op (report left untouched) unless the
// migration is still running (report.Terminal == false) AND pressure is
// actually detected right now. See db.FetchCheckpointPressure's own doc
// comment for the full incident this exists to surface, and
// progress.Report.CheckpointPressureDetected's doc comment for why this
// is a plain, only-shown-when-true bool rather than a persistent status.
//
// Best-effort: a query failure (or an unknown server version) is logged-
// and-ignored, never surfaced as an API error — this is a supplementary,
// situational note, not something that should make an otherwise-
// successful "get migration status" request fail.
func (s *Server) attachCheckpointPressure(ctx context.Context, report *progress.Report) {
	if report.Terminal {
		return
	}
	if s.ConnectionInfo.PostgresVersion == 0 {
		return // version not yet determined (see ConnectionInfo's own doc comment) — nothing to check against
	}

	pressure, err := db.FetchCheckpointPressure(ctx, s.Pool, s.ConnectionInfo.PostgresVersion)
	if err != nil {
		log.Printf("api: failed to check checkpoint pressure: %v", err)
		return
	}
	report.CheckpointPressureDetected = pressure.Pressured
}
