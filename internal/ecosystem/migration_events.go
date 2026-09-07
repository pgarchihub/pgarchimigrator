package ecosystem

// MigrationEventData is the Data payload for every EventTypeMigration*
// event this package emits — deliberately small and stable: just
// enough for a consumer (Lifecycle Intelligence, ArchiMonitor, a human
// reading LogPublisher's log line) to know what happened and to whom,
// without this payload needing to change every time pgArchiMigrator's
// OWN internal Job representation gains a field. This is exactly the
// boundary AC-PF-003 Section 17 draws for adapters in the other
// direction ("what adapters must NOT do: fabricate domain state") —
// this payload is this product's own public contract, not a leak of
// internal/state.Job's shape.
type MigrationEventData struct {
	JobID      string `json:"jobId"`
	SchemaName string `json:"schemaName"`
	TableName  string `json:"tableName"`
	Operation  string `json:"operation"`
	Strategy   string `json:"strategy"`
	Phase      string `json:"phase"`
	// Error is populated only for EventTypeMigrationFailed — omitted
	// (not empty-stringed) otherwise, so a consumer can distinguish
	// "no error field at all" from "an error that happens to be empty."
	Error string `json:"error,omitempty"`
}

// MigrationPhaseChangedData is EventTypeMigrationPhaseChanged's payload
// — the Enterprise-only fine-grained event (see ids.go's own doc
// comment). Embeds MigrationEventData rather than duplicating its
// fields, plus the one thing specific to a phase-level event: which
// phase this transition landed on, redundantly available both here and
// in the embedded Phase field for a consumer that only cares about
// PhaseChanged events and never sees the coarse-grained ones.
type MigrationPhaseChangedData struct {
	MigrationEventData
	// RowsProcessed is included here (and not in the coarse-grained
	// MigrationEventData) because it's the one field genuinely useful
	// mid-migration but not particularly meaningful on the terminal
	// events, where the job's own detail page already shows a final
	// count — an Enterprise consumer watching phase-by-phase progress
	// is exactly who wants to see this changing between events.
	RowsProcessed int64 `json:"rowsProcessed"`
}
