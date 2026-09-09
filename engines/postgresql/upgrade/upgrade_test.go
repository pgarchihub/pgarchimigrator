package upgrade

import (
	"context"
	"testing"
)

// TestConnectionProvider_InterfaceSatisfaction is a compile-time-adjacent
// check — if StaticConnectionProvider ever stops implementing
// ConnectionProvider, this test file fails to compile, which is a more
// useful signal than a runtime test failure for an interface-satisfaction
// regression. Mirrors internal/entitlement's own
// TestChecker_InterfaceSatisfaction exactly, same reasoning.
func TestConnectionProvider_InterfaceSatisfaction(t *testing.T) {
	var _ ConnectionProvider = StaticConnectionProvider{}
}

func TestStaticConnectionProvider_SourceDSN_ReturnsRefUnchanged(t *testing.T) {
	p := StaticConnectionProvider{}
	job := &Job{SourceConnectionRef: "postgresql://user:pass@source-host:5432/mydb"}

	dsn, err := p.SourceDSN(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dsn != job.SourceConnectionRef {
		t.Errorf("expected the ref returned unchanged, got %q", dsn)
	}
}

func TestStaticConnectionProvider_TargetDSN_ReturnsRefUnchanged(t *testing.T) {
	p := StaticConnectionProvider{}
	job := &Job{TargetConnectionRef: "postgresql://user:pass@target-host:5432/mydb"}

	dsn, err := p.TargetDSN(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dsn != job.TargetConnectionRef {
		t.Errorf("expected the ref returned unchanged, got %q", dsn)
	}
}

// TestStaticConnectionProvider_SourceAndTarget_AreIndependent confirms
// the two methods don't accidentally share state or cross-reference the
// wrong field — a real bug class this simple a type could still have if
// SourceDSN/TargetDSN were implemented via a shared helper that mixed up
// which field to read.
func TestStaticConnectionProvider_SourceAndTarget_AreIndependent(t *testing.T) {
	p := StaticConnectionProvider{}
	job := &Job{
		SourceConnectionRef: "postgresql://source",
		TargetConnectionRef: "postgresql://target",
	}

	source, _ := p.SourceDSN(context.Background(), job)
	target, _ := p.TargetDSN(context.Background(), job)

	if source != "postgresql://source" {
		t.Errorf("expected SourceDSN to return the source ref, got %q", source)
	}
	if target != "postgresql://target" {
		t.Errorf("expected TargetDSN to return the target ref, got %q", target)
	}
}

// TestStaticConnectionProvider_ReplicationDSN_FallsBackToSourceConnectionRef
// is the direct regression test for the common case — most deployments
// don't need SourceReplicationRef set at all, since this process and
// the target's own PostgreSQL server usually share the same network
// view of the source.
func TestStaticConnectionProvider_ReplicationDSN_FallsBackToSourceConnectionRef(t *testing.T) {
	p := StaticConnectionProvider{}
	job := &Job{SourceConnectionRef: "postgresql://source", SourceReplicationRef: ""}

	dsn, err := p.ReplicationDSN(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dsn != "postgresql://source" {
		t.Errorf("expected ReplicationDSN to fall back to SourceConnectionRef, got %q", dsn)
	}
}

// TestStaticConnectionProvider_ReplicationDSN_UsesExplicitOverride is the
// direct regression test for the real bug this field exists to fix —
// see Job.SourceReplicationRef's own doc comment: a Docker Compose
// setup where this process reaches source via a host-mapped port but
// the target container needs the Compose network's own hostname.
func TestStaticConnectionProvider_ReplicationDSN_UsesExplicitOverride(t *testing.T) {
	p := StaticConnectionProvider{}
	job := &Job{
		SourceConnectionRef:  "postgresql://localhost:55432/db",
		SourceReplicationRef: "postgresql://pg-logical:5432/db",
	}

	dsn, err := p.ReplicationDSN(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dsn != "postgresql://pg-logical:5432/db" {
		t.Errorf("expected the explicit SourceReplicationRef override, got %q", dsn)
	}
}
