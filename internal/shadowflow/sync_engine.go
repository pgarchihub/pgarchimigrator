// sync_engine.go implements the "Sync Engine" -> "Apply Engine" arrows from
// the Architecture Doc Section 2.1 component diagram: it wires the
// Decoder's decoded WAL stream directly into the ApplyEngine.
package shadowflow

import (
	"context"
	"fmt"

	"github.com/jackc/pglogrepl"
)

// SyncEngine consumes a replication stream via its Decoder and applies
// every decoded change via its ApplyEngine.
type SyncEngine struct {
	Decoder *Decoder
	Apply   *ApplyEngine
}

// Run starts consuming the replication stream from startLSN and applies
// every decoded change until ctx is cancelled or an unrecoverable error
// occurs (Architecture Doc Section 4.1 step 3 "Delta Sync").
func (s *SyncEngine) Run(ctx context.Context, startLSN pglogrepl.LSN) error {
	return s.Decoder.Run(ctx, startLSN, func(ctx context.Context, event ChangeEvent) error {
		if err := s.Apply.Apply(ctx, event); err != nil {
			return fmt.Errorf("sync engine: %w", err)
		}
		return nil
	})
}
