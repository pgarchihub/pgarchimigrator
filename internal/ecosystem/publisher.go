package ecosystem

import (
	"context"
	"encoding/json"
	"log"
)

// Publisher sends a completed Envelope to wherever the ecosystem
// actually receives events (today: nowhere real yet — see
// LogPublisher's own doc comment; eventually: a message broker or
// webhook target this product's operator configures against their
// ArchiConsole/Lifecycle Intelligence deployment). Every call site in
// this codebase that emits an event depends on THIS interface, never on
// a concrete transport — matching entitlement.Checker's own "depend on
// the interface, not on how it's implemented today" reasoning, for the
// exact same future-proofing reason.
type Publisher interface {
	// Publish sends env. Implementations should treat publish failures
	// as non-fatal to the caller's own operation — see Store's own doc
	// comment for why a migration must never fail, stall, or roll back
	// BECAUSE an ecosystem event failed to send. Returning an error here
	// is for the implementation's own observability (so a failure can
	// be logged/counted), not a signal callers are expected to act on.
	Publish(ctx context.Context, env Envelope) error
}

// NoopPublisher discards every event — the correct Publisher for a
// Community-tier instance with no ArchiConsole connection configured at
// all, or for any test that doesn't care about ecosystem events. Always
// returns nil; there is nothing that can fail here.
type NoopPublisher struct{}

func (NoopPublisher) Publish(ctx context.Context, env Envelope) error {
	return nil
}

// LogPublisher writes each envelope as a single JSON log line —
// today's ONLY non-noop Publisher, standing in for a real
// message-broker/webhook transport that doesn't exist yet. This is
// deliberately not a placeholder to be embarrassed about: for a
// self-hosted Community/Enterprise operator without a full
// ArchiConsole/Lifecycle Intelligence deployment, a structured,
// greppable log line of every ecosystem event is itself a genuinely
// useful, working feature (verifiable integration behavior, not a
// stub) — matching this project's own "Community is real" ecosystem
// principle (see ArchiConsole's own Product Principle 9: "Community
// Edition offers real, working integration; it is not merely a
// demo").
// A future HTTPPublisher/BrokerPublisher implements the same Publisher
// interface and nothing else in this codebase changes.
type LogPublisher struct {
	Logger *log.Logger // nil uses the standard library's default logger
}

func (p LogPublisher) Publish(ctx context.Context, env Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	logger := p.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("ecosystem event: %s", body)
	return nil
}
