import type { ReactNode } from "react";

type Tone = "neutral" | "petrol" | "amber" | "coral" | "success";

const toneClasses: Record<Tone, string> = {
  neutral: "bg-ink-100 text-ink-700",
  petrol: "bg-petrol-100 text-petrol-800",
  amber: "bg-amber-100 text-amber-600",
  coral: "bg-coral-100 text-coral-600",
  success: "bg-petrol-100 text-petrol-700",
};

export function Badge({ tone = "neutral", children }: { tone?: Tone; children: ReactNode }) {
  return (
    <span
      className={[
        "inline-flex items-center rounded-full px-2 py-0.5 font-mono text-xs font-medium tracking-tight",
        toneClasses[tone],
      ].join(" ")}
    >
      {children}
    </span>
  );
}
