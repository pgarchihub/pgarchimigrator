package api

import (
	"context"
	"errors"
	"log"

	"github.com/jackc/pgx/v5"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/ddlflow"
	"github.com/pgarchihub/pgarchimigrator/internal/progress"
	"github.com/pgarchihub/pgarchimigrator/internal/shadowflow"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// attachResourceStatus enriches report in place with a LIVE, directly-
// verified list of every transient resource this job's strategy might
// have created (a shadow table, a replication slot, a publication, a
// temporary backfill index) — not a log claiming what happened, an
// actual query confirming what currently exists.
//
// Why this exists — found the hard way, via a real incident during this
// project's own development: a failed SHADOW_TABLE migration left an
// orphaned shadow table behind that internal/reaper could never clean
// up (a sequence had been transferred to it during Preparation, and the
// plain DROP TABLE in failAndCleanup kept failing on a real PostgreSQL
// dependency error — see internal/shadowflow's RevertSequenceOwnership
// doc comment for the full story, now fixed). The orphan sat there,
// completely invisible, until it was found by manually querying
// pg_tables with a psql client — nothing in this project's own UI or API
// ever indicated a problem. A DBA running this tool against a real
// production database needs to be able to see, without needing psql
// access, that a migration's background machinery has actually cleaned
// up after itself — not just trust that it probably did, because the job
// says COMPLETED.
//
// Deliberately only runs for TERMINAL jobs (Terminal==true) — while a
// migration is still actively running, its resources are SUPPOSED to
// exist (that's what "in progress" means), so checking and displaying
// existence there wouldn't distinguish "healthy" from "concerning" the
// way it meaningfully does once the job has finished one way or another.
// Best-effort: a query failure here is logged-and-ignored, never
// surfaced as an API error — this is a supplementary verification, not
// something that should make an otherwise-successful "get migration
// status" request fail.
func (s *Server) attachResourceStatus(ctx context.Context, report *progress.Report, job *state.Job) {
	if !report.Terminal {
		return
	}

	switch job.Strategy {
	case "SHADOW_TABLE":
		report.ResourceStatus = s.shadowTableResourceStatus(ctx, job)
	case "EXPAND_BACKFILL":
		report.ResourceStatus = s.backfillResourceStatus(ctx, job)
	}
}

func (s *Server) shadowTableResourceStatus(ctx context.Context, job *state.Job) []progress.ResourceStatus {
	shadowTable, _, slotName, pubName := shadowflow.ResourceNames(job.ID, job.TableName)
	var statuses []progress.ResourceStatus

	var shadowExists bool
	if err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_tables WHERE schemaname = $1 AND tablename = $2)`,
		job.SchemaName, shadowTable,
	).Scan(&shadowExists); err != nil {
		log.Printf("api: failed to check shadow table existence for job %s: %v", job.ID, err)
	} else {
		statuses = append(statuses, progress.ResourceStatus{Name: "Shadow table", Detail: shadowTable, Exists: shadowExists})
	}

	if _, slotFound, err := db.FetchReplicationLag(ctx, s.Pool, slotName); err != nil {
		log.Printf("api: failed to check replication slot existence for job %s: %v", job.ID, err)
	} else {
		statuses = append(statuses, progress.ResourceStatus{Name: "Replication slot", Detail: slotName, Exists: slotFound})
	}

	var pubExists bool
	if err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_publication WHERE pubname = $1)`, pubName,
	).Scan(&pubExists); err != nil {
		log.Printf("api: failed to check publication existence for job %s: %v", job.ID, err)
	} else {
		statuses = append(statuses, progress.ResourceStatus{Name: "Publication", Detail: pubName, Exists: pubExists})
	}

	return statuses
}

func (s *Server) backfillResourceStatus(ctx context.Context, job *state.Job) []progress.ResourceStatus {
	// The exact index name isn't persisted on the job (see
	// backfillIndexName's own doc comment in internal/ddlflow — it's
	// fully derivable from job.ColumnName + job.ID, so it never needed a
	// dedicated field), so this matches by prefix — the same pattern
	// internal/reaper's own cleanup already uses to find one.
	var indexName string
	err := s.Pool.QueryRow(ctx,
		`SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2 AND indexname LIKE $3 LIMIT 1`,
		job.SchemaName, job.TableName, ddlflow.BackfillIndexPrefix+"%",
	).Scan(&indexName)
	switch {
	case err == nil:
		return []progress.ResourceStatus{{Name: "Temporary backfill index", Detail: indexName, Exists: true}}
	case errors.Is(err, pgx.ErrNoRows):
		// The healthy, common case — nothing left behind. Still reported
		// (Exists: false), not omitted — see ResourceStatus's own doc
		// comment for why a positive "checked, and it's clean" is shown
		// rather than staying silent.
		return []progress.ResourceStatus{{Name: "Temporary backfill index", Detail: ddlflow.BackfillIndexPrefix + "*", Exists: false}}
	default:
		log.Printf("api: failed to check backfill index existence for job %s: %v", job.ID, err)
		return nil
	}
}
