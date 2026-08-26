import { describe, expect, it } from "vitest";
import { formatBytes, formatDuration, formatRowCount, isZeroTime } from "./format";

describe("isZeroTime", () => {
  it("treats Go's zero time.Time sentinel as zero", () => {
    expect(isZeroTime("0001-01-01T00:00:00Z")).toBe(true);
  });

  it("treats an empty string as zero", () => {
    expect(isZeroTime("")).toBe(true);
  });

  it("treats undefined/null as zero", () => {
    expect(isZeroTime(undefined)).toBe(true);
    expect(isZeroTime(null)).toBe(true);
  });

  it("treats an unparseable string as zero (defensive default)", () => {
    expect(isZeroTime("not-a-date")).toBe(true);
  });

  it("does not treat a real, recent timestamp as zero", () => {
    expect(isZeroTime("2026-08-20T10:00:00Z")).toBe(false);
  });
});

describe("formatDuration", () => {
  it("formats sub-minute durations as seconds only, with two decimal places", () => {
    expect(formatDuration(45_000)).toBe("45.00s");
  });

  it("formats multi-minute durations with minutes and seconds", () => {
    expect(formatDuration(5 * 60_000 + 30_000)).toBe("5m 30.00s");
  });

  it("formats multi-hour durations with hours, minutes, and seconds", () => {
    expect(formatDuration(2 * 3_600_000 + 15 * 60_000 + 5_000)).toBe("2h 15m 5.00s");
  });

  it("clamps negative durations to 0.00s rather than showing a negative value", () => {
    // A negative duration should never legitimately happen (it would mean
    // UpdatedAt is before CreatedAt), but if clock skew or a bad job
    // record ever produced one, showing "-5s" would be more confusing
    // than showing 0s.
    expect(formatDuration(-5000)).toBe("0.00s");
  });

  it("formats exactly zero as 0.00s", () => {
    expect(formatDuration(0)).toBe("0.00s");
  });

  // Direct regression test for the real bug this fix addresses: a
  // Math.floor(ms / 1000) implementation collapsed ANY sub-second
  // duration down to "0s" — a 50ms DIRECT_DDL migration and one that
  // hadn't started yet both rendered identically, with no way to tell
  // a genuinely fast (but real) operation from one showing no progress
  // at all.
  it("does not collapse a sub-second duration down to a bare '0s' — shows millisecond-level precision instead", () => {
    expect(formatDuration(50)).toBe("0.05s");
  });

  it("shows millisecond precision within a multi-minute duration too", () => {
    expect(formatDuration(5 * 60_000 + 30_250)).toBe("5m 30.25s");
  });
});

describe("formatRowCount", () => {
  it("adds thousands separators for large numbers", () => {
    expect(formatRowCount(1234567)).toBe((1234567).toLocaleString());
  });

  it("formats small numbers without separators", () => {
    expect(formatRowCount(42)).toBe("42");
  });

  it("formats zero", () => {
    expect(formatRowCount(0)).toBe("0");
  });
});

describe("formatBytes", () => {
  it("formats sub-1024 values as bytes", () => {
    expect(formatBytes(500)).toBe("500 B");
  });

  it("formats zero as bytes", () => {
    expect(formatBytes(0)).toBe("0 B");
  });

  it("formats kilobytes with one decimal place", () => {
    expect(formatBytes(1024)).toBe("1.0 KB");
  });

  it("formats megabytes (a typical replication lag reading)", () => {
    expect(formatBytes(47_185_920)).toBe("45.0 MB");
  });

  it("formats gigabytes for a large lag reading", () => {
    expect(formatBytes(4_294_967_296)).toBe("4.0 GB");
  });

  it("caps at TB rather than continuing to an unlabeled unit", () => {
    // An implausibly large value — this just confirms the unit loop
    // stops at the last defined unit instead of indexing out of bounds.
    expect(formatBytes(1024 ** 5)).toBe("1024.0 TB");
  });
});
