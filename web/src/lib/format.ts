// Go's zero time.Time ("never set") serializes via encoding/json as
// "0001-01-01T00:00:00Z" — a syntactically valid date the UI must not
// render as if it were real. Any parsed year before this threshold is
// treated as "not actually set".
const ZERO_TIME_THRESHOLD_YEAR = 1970;

export function isZeroTime(iso: string | undefined | null): boolean {
  if (!iso) return true;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) || d.getUTCFullYear() < ZERO_TIME_THRESHOLD_YEAR;
}

export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// formatDuration renders a duration as an h/m/s breakdown, with the
// seconds component always showing two decimal places — e.g. "2h 15m
// 5.00s", "5m 30.25s", "0.05s". Previously used Math.floor(ms / 1000),
// which silently discarded anything under a second: a 50ms DIRECT_DDL
// migration and one that hadn't started yet both rendered as "0s",
// indistinguishable from each other. Always showing two decimals (rather
// than only for sub-second durations) keeps the format consistent
// regardless of how long the operation took.
export function formatDuration(ms: number): string {
  const totalSeconds = Math.max(0, ms) / 1000;
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;

  const parts: string[] = [];
  if (hours > 0) parts.push(`${hours}h`);
  if (hours > 0 || minutes > 0) parts.push(`${minutes}m`);
  parts.push(`${seconds.toFixed(2)}s`);
  return parts.join(" ");
}

export function formatRowCount(n: number): string {
  return n.toLocaleString();
}

// formatBytes renders a byte count (e.g. replication lag) in the
// largest whole unit that keeps the number readable at a glance — "45
// MB" rather than "47185920 bytes" or an over-precise "45.00 MB".
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex++;
  }
  return `${value.toFixed(1)} ${units[unitIndex]}`;
}
