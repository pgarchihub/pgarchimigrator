import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { PhaseTrack } from "./PhaseTrack";
import type { StageView } from "../lib/types";

function stage(phase: StageView["Phase"], status: StageView["Status"]): StageView {
  return { Phase: phase, Status: status };
}

describe("PhaseTrack", () => {
  it("renders one solid circle per stage, plus a pulse ring for the current one", () => {
    const stages = [
      stage("PREPARATION", "DONE"),
      stage("SYNCING", "CURRENT"),
      stage("VALIDATING", "PENDING"),
      stage("COMPLETED", "PENDING"),
    ];
    const { container } = render(<PhaseTrack stages={stages} />);
    expect(container.querySelectorAll("circle").length).toBe(stages.length + 1);
  });

  it("renders no pulse ring when no stage is current (e.g. a terminal job)", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("COMPLETED", "DONE")];
    const { container } = render(<PhaseTrack stages={stages} />);
    expect(container.querySelectorAll("circle").length).toBe(stages.length);
  });

  it("colors a done stage petrol and a pending stage ink", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("COMPLETED", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    const circles = Array.from(container.querySelectorAll("circle"));
    expect(circles[0]).toHaveClass("fill-petrol-600");
    expect(circles[1]).toHaveClass("fill-ink-400");
  });

  it("colors the current stage amber and adds a motion-safe pulsing ring", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("SYNCING", "CURRENT")];
    const { container } = render(<PhaseTrack stages={stages} />);
    expect(container.querySelector("circle.fill-amber-400")).not.toBeNull();
    expect(container.querySelector("circle.motion-safe\\:animate-ping")).not.toBeNull();
  });

  // Direct regression test for a real usability gap: a DONE station used
  // to be indistinguishable in SHAPE from a PENDING one, relying purely
  // on color — not reliable for anyone with a color vision deficiency,
  // and doesn't read as "complete" at a glance the way a checkmark does.
  it("draws a checkmark on a completed station in full (non-compact) mode", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("SYNCING", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    // A checkmark is drawn as a stroked (not filled) path, distinct from
    // the solid-fill station circles/text — this SVG has no other stroked
    // paths in the healthy/pending case, so its mere presence confirms
    // the checkmark specifically.
    const checkmarks = container.querySelectorAll("path");
    expect(checkmarks.length).toBe(1);
  });

  it("does not draw a checkmark on a pending or current station", () => {
    const stages = [stage("PREPARATION", "CURRENT"), stage("SYNCING", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    expect(container.querySelectorAll("path").length).toBe(0);
  });

  it("draws one checkmark per completed station, not just the first", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("SYNCING", "DONE"), stage("VALIDATING", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    expect(container.querySelectorAll("path").length).toBe(2);
  });

  // Compact mode (dashboard table rows) deliberately skips the checkmark
  // — see PhaseTrack's own comment: at compact's much smaller radius a
  // checkmark this small would likely render as an illegible smudge
  // rather than add clarity, so it keeps the simpler solid-circle
  // treatment instead.
  it("does not draw a checkmark in compact mode", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("SYNCING", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} compact />);
    expect(container.querySelectorAll("path").length).toBe(0);
  });

  it("colors a segment petrol once its preceding station is done, ink otherwise", () => {
    // PREPARATION is done -> the segment into SYNCING is petrol.
    // SYNCING is only "CURRENT" (not done yet) -> the segment into
    // VALIDATING must still be ink, not prematurely petrol.
    const stages = [stage("PREPARATION", "DONE"), stage("SYNCING", "CURRENT"), stage("VALIDATING", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    const lines = Array.from(container.querySelectorAll("line"));
    expect(lines[0]).toHaveClass("text-petrol-500");
    expect(lines[1]).toHaveClass("text-ink-400");
  });

  it("marks a ROLLBACK_WINDOW station with a dashed ring regardless of its status", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("ROLLBACK_WINDOW", "PENDING"), stage("COMPLETED", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    const dashed = Array.from(container.querySelectorAll("circle")).find(
      (c) => c.getAttribute("stroke-dasharray") === "2 2",
    );
    expect(dashed).toBeDefined();
  });

  it("does not add a dashed ring to non-rollback-window stations", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("COMPLETED", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    const dashed = Array.from(container.querySelectorAll("circle")).find(
      (c) => c.getAttribute("stroke-dasharray") === "2 2",
    );
    expect(dashed).toBeUndefined();
  });

  it("omits phase-name text labels in compact mode (dashboard row usage)", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("COMPLETED", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} compact />);
    expect(container.querySelectorAll("text").length).toBe(0);
  });

  it("shows one text label per stage in full (non-compact) mode", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("COMPLETED", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    expect(container.querySelectorAll("text").length).toBe(stages.length);
  });

  it("sets an aria-label summarizing how many stages are complete", () => {
    const stages = [
      stage("PREPARATION", "DONE"),
      stage("SYNCING", "DONE"),
      stage("VALIDATING", "CURRENT"),
      stage("COMPLETED", "PENDING"),
    ];
    const { container } = render(<PhaseTrack stages={stages} />);
    expect(container.querySelector("svg")).toHaveAttribute(
      "aria-label",
      "Migration progress: 2 of 4 phases complete",
    );
  });

  // Regression coverage for a real mobile-responsiveness bug: full mode
  // previously used width:100% unconditionally, so the SVG (and its
  // text labels) would shrink to fit ANY container — including a narrow
  // phone viewport — making a many-station track's phase names illegible
  // instead of triggering the horizontal-scroll wrapper already present
  // on the page that renders this (MigrationDetail).
  describe("responsive sizing", () => {
    it("full mode gets a fixed pixel minWidth, not width:100%", () => {
      const stages = [stage("PREPARATION", "DONE"), stage("COMPLETED", "PENDING")];
      const { container } = render(<PhaseTrack stages={stages} />);
      const svg = container.querySelector("svg") as SVGSVGElement;
      expect(svg.style.minWidth).not.toBe("");
      expect(svg.className.baseVal).not.toContain("w-full");
    });

    it("full mode's minWidth scales up with more stages (so a 9-station SHADOW_TABLE track stays readable)", () => {
      const twoStages = [stage("PREPARATION", "DONE"), stage("COMPLETED", "PENDING")];
      // The real 9-phase SHADOW_TABLE pipeline (see internal/progress's
      // pipelineFor) — reused here rather than synthetic phase names, so
      // this stays a faithful stand-in for the actual worst case.
      const nineStages = [
        "PREFLIGHT",
        "PREPARATION",
        "SYNCING",
        "DELTA_SYNC",
        "VALIDATING",
        "SWAPPING",
        "ROLLBACK_WINDOW",
        "CLEANUP",
        "COMPLETED",
      ].map((p) => stage(p as StageView["Phase"], "PENDING"));
      const { container: c1 } = render(<PhaseTrack stages={twoStages} />);
      const { container: c2 } = render(<PhaseTrack stages={nineStages} />);
      const width1 = parseFloat((c1.querySelector("svg") as SVGSVGElement).style.minWidth);
      const width2 = parseFloat((c2.querySelector("svg") as SVGSVGElement).style.minWidth);
      expect(width2).toBeGreaterThan(width1);
    });

    it("compact mode still uses width:100% (dashboard table cells should never overflow)", () => {
      const stages = [stage("PREPARATION", "DONE"), stage("COMPLETED", "PENDING")];
      const { container } = render(<PhaseTrack stages={stages} compact />);
      const svg = container.querySelector("svg") as SVGSVGElement;
      expect(svg.className.baseVal).toContain("w-full");
      expect(svg.style.minWidth).toBe("");
    });
  });

  // role="img" + aria-label on the <svg> already gives assistive tech a
  // complete, single summary of this component — without aria-hidden on
  // the actual visual content, a screen reader could ALSO try to read out
  // every individual station's <text> label separately, which would be
  // redundant with (and less clear than) the aria-label summary alone.
  it("hides all decorative content from assistive tech, keeping only the svg's own aria-label", () => {
    const stages = [stage("PREPARATION", "DONE"), stage("SYNCING", "CURRENT"), stage("COMPLETED", "PENDING")];
    const { container } = render(<PhaseTrack stages={stages} />);
    const svg = container.querySelector("svg") as SVGSVGElement;

    expect(svg).toHaveAttribute("role", "img");
    expect(svg.getAttribute("aria-label")).toBeTruthy();

    const hiddenGroup = svg.querySelector(":scope > g[aria-hidden='true']");
    expect(hiddenGroup).not.toBeNull();
    // Every circle/line/text this component renders must live INSIDE that
    // hidden group — none directly under the svg itself, or it would
    // still be exposed to assistive tech outside the aria-hidden wrapper.
    expect(svg.querySelectorAll(":scope > circle, :scope > line, :scope > text").length).toBe(0);
    expect(hiddenGroup?.querySelectorAll("circle, line, text").length).toBeGreaterThan(0);
  });
});
