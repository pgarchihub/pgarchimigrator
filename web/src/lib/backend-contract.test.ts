import { describe, expect, it } from "vitest";
import type { StageView } from "./types";

// This file exists because of a real, user-reported bug: StageView's
// Status type (and every comparison against it, in PhaseTrack.tsx and
// MigrationDetail.tsx) used lowercase "done"/"current"/"pending" for a
// long stretch of this project, while the real backend
// (internal/progress's StageStatus constants) has ALWAYS sent uppercase
// "DONE"/"CURRENT"/"PENDING". Every comparison silently never matched,
// so a completed migration's phase track and step list both rendered as
// if every stage were still pending.
//
// The reason this went undetected for so long: every test fixture in
// this codebase ALSO used the same wrong lowercase literals, so the
// tests were internally self-consistent with the bug rather than
// validating against the real contract. A test suite that only checks
// "does my code agree with my own test data" can never catch this class
// of error — it needs at least one test that hardcodes the ACTUAL
// correct value independently, sourced from the Go side's real constant
// definitions, not from whatever the TypeScript side currently assumes.
//
// That's what this file is. It deliberately does NOT import anything
// from ui/PhaseTrack.tsx or routes/MigrationDetail.tsx — if someone
// "fixes" this test by making it agree with a re-introduced bug instead
// of fixing the bug, that's a visible, deliberate act, not an accident.
describe("StageView.Status backend contract", () => {
  it("matches internal/progress's StageStatus constants exactly (DONE / CURRENT / PENDING, uppercase)", () => {
    // See internal/progress/progress.go:
    //   StageDone    StageStatus = "DONE"
    //   StageCurrent StageStatus = "CURRENT"
    //   StagePending StageStatus = "PENDING"
    const knownCorrectBackendValues: StageView["Status"][] = ["DONE", "CURRENT", "PENDING"];

    for (const value of knownCorrectBackendValues) {
      // This assignment only compiles if StageView["Status"] actually
      // includes this literal — a lowercase-typed StageView would fail
      // `tsc -b` right here, which is exactly the safety net that was
      // missing before.
      const stage: StageView = { Phase: "PREPARATION", Status: value };
      expect(stage.Status).toBe(value);
    }
  });

  it("does not accept the old, incorrect lowercase values", () => {
    // @ts-expect-error — "done" (lowercase) must NOT be a valid Status;
    // if this ts-expect-error becomes unnecessary (i.e. the assignment
    // below starts compiling), the type has regressed back to accepting
    // the wrong values, and this test file's whole purpose is defeated.
    const stage: StageView = { Phase: "PREPARATION", Status: "done" };
    expect(stage).toBeDefined();
  });
});
