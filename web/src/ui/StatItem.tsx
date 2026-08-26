import type { ReactNode } from "react";

export function StatItem({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs uppercase tracking-wide text-ink-400">{label}</span>
      <span className="font-mono text-sm text-ink-700">{value}</span>
    </div>
  );
}
