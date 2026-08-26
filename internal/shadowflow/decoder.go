// decoder.go implements the "Decoder" component from Architecture Doc
// Section 3.2/4.1: it reads the raw pgoutput binary protocol off a
// replication-mode connection and turns it into structured ChangeEvent
// values. It also owns the keepalive/status-update handling required by
// the replication protocol (without periodic StandbyStatusUpdate messages,
// PostgreSQL will eventually time out the connection).
//
// This single type deliberately combines what the architecture diagram
// draws as two boxes ("Decoder" -> "Sync Engine"): the caller supplies a
// handler function that IS the Sync Engine/Apply Engine bridge (see
// sync_engine.go). Splitting them into separate Go types added indirection
// without changing behavior, since the protocol loop and the "what do I do
// with a decoded change" step are tightly coupled by the need to only
// confirm an LSN once the corresponding change has actually been applied.
package shadowflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// ChangeKind identifies the kind of row-level change a ChangeEvent carries.
type ChangeKind string

const (
	ChangeInsert ChangeKind = "INSERT"
	ChangeUpdate ChangeKind = "UPDATE"
	ChangeDelete ChangeKind = "DELETE"
)

// ChangeEvent is a single decoded row-level change, ready for
// ApplyEngine.Apply. Columns/Values holds the new row image for
// INSERT/UPDATE (columns whose value pgoutput reported as "unchanged
// toast" are omitted — see decodeTuple). For DELETE, Columns/Values holds
// the old row's replica-identity columns (normally just the primary key).
type ChangeEvent struct {
	Kind    ChangeKind
	Schema  string
	Table   string
	Columns []string
	Values  []any // nil entries represent SQL NULL
	LSN     pglogrepl.LSN
}

// standbyMessageTimeout is how often a StandbyStatusUpdate is sent to
// PostgreSQL to confirm progress and keep the connection alive.
const standbyMessageTimeout = 10 * time.Second

// Decoder consumes a logical replication stream and dispatches decoded
// changes to a handler function.
type Decoder struct {
	ReplicationDSN  string
	SlotName        string
	PublicationName string
}

// Run connects to the replication slot, starts streaming from startLSN, and
// calls handler for every decoded INSERT/UPDATE/DELETE. Run blocks until
// ctx is cancelled or an unrecoverable error occurs. The handler's error,
// if any, is treated as unrecoverable and stops the loop — the caller
// (SyncEngine) is expected to handle retries/backoff at a higher level
// rather than have the Decoder silently skip failed changes, since
// skipping a change would silently desynchronize the shadow table.
//
// Only after handler returns nil for a given WAL record does Run advance
// the confirmed LSN sent in the next StandbyStatusUpdate — this is what
// makes it safe to resume from the last successfully applied position
// after a crash (the slot will replay from the last confirmed LSN).
func (d *Decoder) Run(ctx context.Context, startLSN pglogrepl.LSN, handler func(ctx context.Context, event ChangeEvent) error) error {
	conn, err := pgconn.Connect(ctx, d.ReplicationDSN)
	if err != nil {
		return fmt.Errorf("failed to open a replication-mode connection: %w", err)
	}
	defer conn.Close(ctx)

	pluginArguments := []string{"proto_version '1'", fmt.Sprintf("publication_names '%s'", d.PublicationName)}
	if err := pglogrepl.StartReplication(ctx, conn, d.SlotName, startLSN, pglogrepl.StartReplicationOptions{PluginArgs: pluginArguments}); err != nil {
		return fmt.Errorf("failed to start replication on slot %q: %w", d.SlotName, err)
	}

	clientXLogPos := startLSN
	relations := map[uint32]*pglogrepl.RelationMessage{}
	nextStandbyDeadline := time.Now().Add(standbyMessageTimeout)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if time.Now().After(nextStandbyDeadline) {
			if err := pglogrepl.SendStandbyStatusUpdate(ctx, conn,
				pglogrepl.StandbyStatusUpdate{
					WALWritePosition: clientXLogPos,
					WALFlushPosition: clientXLogPos,
					WALApplyPosition: clientXLogPos,
				},
			); err != nil {
				return fmt.Errorf("failed to send standby status update: %w", err)
			}
			nextStandbyDeadline = time.Now().Add(standbyMessageTimeout)
		}

		recvCtx, cancel := context.WithDeadline(ctx, nextStandbyDeadline)
		rawMsg, err := conn.ReceiveMessage(recvCtx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue // no message before the standby deadline; loop to send a status update
			}
			if errors.Is(err, context.Canceled) {
				return ctx.Err()
			}
			return fmt.Errorf("failed to receive replication message: %w", err)
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			return fmt.Errorf("replication stream returned an error: %s (%s)", errMsg.Message, errMsg.Code)
		}

		copyData, ok := rawMsg.(*pgproto3.CopyData)
		if !ok {
			continue // unexpected message type; ignore per pglogrepl's documented pattern
		}

		switch copyData.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(copyData.Data[1:])
			if err != nil {
				return fmt.Errorf("failed to parse keepalive message: %w", err)
			}
			if pkm.ReplyRequested {
				nextStandbyDeadline = time.Time{} // force an immediate status update on the next loop iteration
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(copyData.Data[1:])
			if err != nil {
				return fmt.Errorf("failed to parse XLogData: %w", err)
			}

			event, err := decodeLogicalMessage(xld.WALData, relations, xld.WALStart)
			if err != nil {
				return fmt.Errorf("failed to decode logical replication message: %w", err)
			}

			if event != nil {
				if err := handler(ctx, *event); err != nil {
					return fmt.Errorf("handler failed to apply change (LSN %s): %w", xld.WALStart, err)
				}
			}

			if xld.WALStart > clientXLogPos {
				clientXLogPos = xld.WALStart
			}
		}
	}
}

// decodeLogicalMessage parses a single pgoutput binary message. It returns
// a non-nil *ChangeEvent for INSERT/UPDATE/DELETE messages, and nil for
// bookkeeping-only messages (BEGIN, COMMIT, RELATION, TYPE, ORIGIN,
// TRUNCATE) that don't themselves represent a row change to apply.
// RELATION messages are cached in `relations` so subsequent
// Insert/Update/Delete messages referencing the same RelationID can be
// decoded into named columns.
func decodeLogicalMessage(walData []byte, relations map[uint32]*pglogrepl.RelationMessage, lsn pglogrepl.LSN) (*ChangeEvent, error) {
	msg, err := pglogrepl.Parse(walData)
	if err != nil {
		return nil, err
	}

	switch m := msg.(type) {
	case *pglogrepl.RelationMessage:
		relations[m.RelationID] = m
		return nil, nil

	case *pglogrepl.InsertMessage:
		rel, ok := relations[m.RelationID]
		if !ok {
			return nil, fmt.Errorf("received INSERT for unknown relation ID %d (missing RELATION message)", m.RelationID)
		}
		cols, vals, err := decodeTuple(rel, m.Tuple)
		if err != nil {
			return nil, fmt.Errorf("failed to decode INSERT tuple: %w", err)
		}
		return &ChangeEvent{Kind: ChangeInsert, Schema: rel.Namespace, Table: rel.RelationName, Columns: cols, Values: vals, LSN: lsn}, nil

	case *pglogrepl.UpdateMessage:
		rel, ok := relations[m.RelationID]
		if !ok {
			return nil, fmt.Errorf("received UPDATE for unknown relation ID %d (missing RELATION message)", m.RelationID)
		}
		cols, vals, err := decodeTuple(rel, m.NewTuple)
		if err != nil {
			return nil, fmt.Errorf("failed to decode UPDATE tuple: %w", err)
		}
		return &ChangeEvent{Kind: ChangeUpdate, Schema: rel.Namespace, Table: rel.RelationName, Columns: cols, Values: vals, LSN: lsn}, nil

	case *pglogrepl.DeleteMessage:
		rel, ok := relations[m.RelationID]
		if !ok {
			return nil, fmt.Errorf("received DELETE for unknown relation ID %d (missing RELATION message)", m.RelationID)
		}
		if m.OldTuple == nil {
			return nil, fmt.Errorf("received DELETE with no OldTuple — the source table must have REPLICA IDENTITY DEFAULT or FULL (Architecture Doc Section 3.2)")
		}
		cols, vals, err := decodeTuple(rel, m.OldTuple)
		if err != nil {
			return nil, fmt.Errorf("failed to decode DELETE tuple: %w", err)
		}
		return &ChangeEvent{Kind: ChangeDelete, Schema: rel.Namespace, Table: rel.RelationName, Columns: cols, Values: vals, LSN: lsn}, nil

	default:
		// BeginMessage, CommitMessage, TypeMessage, OriginMessage,
		// TruncateMessage: no row-level change to apply.
		return nil, nil
	}
}

// decodeTuple maps a pgoutput TupleData onto the column names from the
// corresponding RelationMessage. Columns reported as "unchanged TOAST"
// (DataType == 'u') are deliberately OMITTED from the result rather than
// given a zero value: we genuinely don't know their current value, and
// including them with a wrong value would silently corrupt the shadow
// table. ApplyEngine's UPSERT is built to only SET the columns present
// here, leaving any omitted column untouched on the target row.
func decodeTuple(rel *pglogrepl.RelationMessage, tuple *pglogrepl.TupleData) ([]string, []any, error) {
	if tuple == nil {
		return nil, nil, fmt.Errorf("nil tuple for relation %s.%s", rel.Namespace, rel.RelationName)
	}
	if len(tuple.Columns) != len(rel.Columns) {
		return nil, nil, fmt.Errorf("tuple column count (%d) does not match relation column count (%d) for %s.%s",
			len(tuple.Columns), len(rel.Columns), rel.Namespace, rel.RelationName)
	}

	cols := make([]string, 0, len(tuple.Columns))
	vals := make([]any, 0, len(tuple.Columns))

	for i, col := range tuple.Columns {
		name := rel.Columns[i].Name
		switch col.DataType {
		case 'u': // unchanged TOAST value — omit, see doc comment above
			continue
		case 'n': // SQL NULL
			cols = append(cols, name)
			vals = append(vals, nil)
		case 't': // text-encoded value
			cols = append(cols, name)
			vals = append(vals, string(col.Data))
		default:
			return nil, nil, fmt.Errorf("unsupported pgoutput column data type %q for column %s", string(col.DataType), name)
		}
	}

	return cols, vals, nil
}
