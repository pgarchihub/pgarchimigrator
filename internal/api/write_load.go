package api

import (
	"net/http"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
)

// writeLoadSampleDuration is how long handleEstimateWriteLoad blocks the
// caller while measuring — long enough to smooth out momentary noise (a
// single burst of writes), short enough to still be a reasonable "check
// before you start" step rather than a real wait. This is exactly why
// this whole feature is opt-in: the New Migration screen only calls this
// when the person explicitly asks for a write-load check, never as part
// of every migration.
const writeLoadSampleDuration = 10 * time.Second

// writeLoadCautionThresholdBytesPerSecond is a conservative, clearly-
// labeled bar for flagging "this write volume is worth being aware of
// before a SHADOW_TABLE migration" — NOT a precise, benchmarked
// "will/won't converge" verdict. This project's own real incident (see
// internal/api's attachReplicationLag doc comment) happened under an
// aggressive, synthetic load-testing write pattern; the exact throughput
// a real ApplyEngine can sustain depends heavily on hardware, network,
// and row shape this project has no broad enough benchmark data to
// characterize precisely. 5 MB/s of SUSTAINED WAL generation is already
// a genuinely busy table for most deployments, and crossing it is worth
// a heads-up — not a claim that anything below it is definitely safe, or
// anything above it definitely isn't.
const writeLoadCautionThresholdBytesPerSecond = 5 * 1024 * 1024

type writeLoadEstimate struct {
	BytesPerSecond float64 `json:"bytesPerSecond"`
	SampleSeconds  float64 `json:"sampleSeconds"`
	// Caution is advisory only — see writeLoadCautionThresholdBytesPerSecond's
	// own doc comment. Never used to block a migration from starting;
	// the decision is always left to whoever's reading this.
	Caution bool `json:"caution"`
}

// handleEstimateWriteLoad samples the CURRENT WAL generation rate over
// writeLoadSampleDuration and reports it — see db.SampleWALGenerationRate's
// doc comment for the full "why". Deliberately its own, explicitly-
// triggered endpoint (not folded into the existing preview endpoint,
// which returns near-instantly): a caller that wants a fast preview must
// not be forced to wait 10 seconds for a check they didn't ask for.
func (s *Server) handleEstimateWriteLoad(w http.ResponseWriter, r *http.Request) {
	rate, err := db.SampleWALGenerationRate(r.Context(), s.Pool, writeLoadSampleDuration)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, writeLoadEstimate{
		BytesPerSecond: rate,
		SampleSeconds:  writeLoadSampleDuration.Seconds(),
		Caution:        isWriteLoadCautionWorthy(rate),
	})
}

// isWriteLoadCautionWorthy is the pure threshold logic
// handleEstimateWriteLoad applies to the sampled rate — split out so
// it's testable without a real PostgreSQL connection (matching
// internal/db's isCheckpointPressured, the same pattern used for the
// checkpoint-pressure indicator's own threshold).
func isWriteLoadCautionWorthy(bytesPerSecond float64) bool {
	return bytesPerSecond >= writeLoadCautionThresholdBytesPerSecond
}
