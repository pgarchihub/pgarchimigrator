import type { StageView } from "../lib/types";

interface PhaseTrackProps {
  stages: StageView[];
  /** Compact renders a small inline version for dashboard table rows. */
  compact?: boolean;
}

// PhaseTrack is this product's signature visual element — a schematic
// "station track" rendering of a migration's phase pipeline, carried
// through the dashboard (compact), the migration detail page (full), and
// the new-migration preview. It exists because the phase timeline
// (Preparation -> Syncing -> Validating -> Rollback Window -> Completed)
// is genuinely the core mental model of this product, not decoration.
export function PhaseTrack({ stages, compact = false }: PhaseTrackProps) {
  const n = stages.length;
  const gap = compact ? 32 : 116;
  const width = Math.max(n * gap, gap);
  const cy = compact ? 10 : 18;
  const height = compact ? 20 : 56;
  const radius = compact ? 4 : 8;

  return (
    <svg
      viewBox={`0 0 ${width} ${height}`}
      // Compact mode (dashboard rows) always fills its cell at 100% width —
      // there's no text to become illegible, so proportional shrinking is
      // fine. Full mode (Migration Detail) is given a fixed pixel
      // minWidth instead of width:100%: without this, the SVG would keep
      // shrinking to fit any container, including a narrow phone
      // viewport, making a 9-station SHADOW_TABLE track's phase-name text
      // shrink into illegibility. With a real minWidth, it instead
      // overflows its container at a readable size, and the
      // overflow-x-auto wrapper already in MigrationDetail's CardBody
      // picks up the slack with a horizontal scrollbar.
      //
      // text-ink-400 (not the lighter ink-200/300) is deliberate: this is
      // the "not yet reached" color for BOTH the connecting segment lines
      // and the pending station circles below — a real, computed WCAG
      // check found ink-200 sits at only 1.69:1 against white, far under
      // the 3:1 minimum for meaningful graphical/UI indicators (WCAG
      // 1.4.11). ink-400 (5.12:1) was already the closest existing token
      // that actually clears the bar, reused here rather than inventing
      // a new one-off color just for this component.
      className={compact ? "w-full text-ink-400" : "text-ink-400"}
      style={compact ? { height } : { height, minWidth: width }}
      role="img"
      aria-label={`Migration progress: ${stages.filter((s) => s.Status === "DONE").length} of ${n} phases complete`}
    >
      {/* role="img" + aria-label above already gives assistive tech the
          complete summary as one atomic image — everything inside is
          aria-hidden so a screen reader doesn't ALSO try to read out
          every individual <text> phase label separately, which would be
          redundant with (and less clear than) the aria-label. */}
      <g aria-hidden="true">
        {stages.slice(1).map((s, idx) => {
          const prev = stages[idx];
          const x1 = gap / 2 + idx * gap;
          const x2 = gap / 2 + (idx + 1) * gap;
          const isComplete = prev.Status === "DONE";
          return (
            <line
              key={`seg-${s.Phase}`}
              x1={x1}
              y1={cy}
              x2={x2}
              y2={cy}
              stroke="currentColor"
              className={isComplete ? "text-petrol-500" : "text-ink-400"}
              strokeWidth={compact ? 2 : 3}
            />
          );
        })}
        {stages.map((s, i) => {
          const cx = gap / 2 + i * gap;
          const isRollbackWindow = s.Phase === "ROLLBACK_WINDOW";
          return (
            <g key={s.Phase}>
              {s.Status === "CURRENT" && (
                <circle
                  cx={cx}
                  cy={cy}
                  r={radius + (compact ? 4 : 7)}
                  className="fill-none stroke-amber-300 motion-safe:animate-ping"
                  strokeWidth={1.5}
                />
              )}
              <circle
                cx={cx}
                cy={cy}
                r={radius}
                className={
                  s.Status === "DONE"
                    ? "fill-petrol-600"
                    : s.Status === "CURRENT"
                      ? "fill-amber-400"
                      : "fill-ink-400"
                }
                stroke={isRollbackWindow ? "currentColor" : "none"}
                strokeWidth={isRollbackWindow ? 1.5 : 0}
                strokeDasharray={isRollbackWindow ? "2 2" : undefined}
              />
              {/* A checkmark drawn directly on a completed station — see
                  this component's own history: previously every DONE
                  station was just a solid filled circle, identical in
                  shape to a PENDING one and distinguishable only by
                  color, which isn't reliable for anyone with a color
                  vision deficiency and doesn't read as "done" at a
                  glance the way a checkmark does. Only drawn in the
                  full (non-compact) view — at compact's much smaller
                  radius (4px vs 8px) a checkmark this small would likely
                  render as an illegible smudge rather than add clarity. */}
              {!compact && s.Status === "DONE" && (
                <path
                  d={`M${cx - radius * 0.5} ${cy} L${cx - radius * 0.1} ${cy + radius * 0.45} L${cx + radius * 0.55} ${cy - radius * 0.45}`}
                  stroke="white"
                  strokeWidth={1.6}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  fill="none"
                />
              )}
              {!compact && (
                <text
                  x={cx}
                  y={cy + radius + 18}
                  textAnchor="middle"
                  className="fill-ink-500 font-mono uppercase"
                  style={{ fontSize: 9, letterSpacing: "0.02em" }}
                >
                  {s.Phase.replace(/_/g, " ")}
                </text>
              )}
            </g>
          );
        })}
      </g>
    </svg>
  );
}
