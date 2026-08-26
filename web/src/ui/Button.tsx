import { type ButtonHTMLAttributes, forwardRef } from "react";

type Variant = "primary" | "secondary" | "danger" | "ghost";

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
}

const variantClasses: Record<Variant, string> = {
  primary: "bg-petrol-700 text-white hover:bg-petrol-800 focus-visible:ring-petrol-500",
  secondary:
    "bg-white text-ink-700 border border-ink-200 hover:border-ink-300 hover:bg-ink-50 focus-visible:ring-petrol-500",
  danger: "bg-coral-500 text-white hover:bg-coral-600 focus-visible:ring-coral-400",
  ghost: "bg-transparent text-ink-600 hover:bg-ink-100 focus-visible:ring-petrol-500",
};

// buttonClasses is exported so non-<button> elements that need to LOOK
// like a button (most commonly a react-router <Link>, since nesting a
// <button> inside an <a> is invalid HTML) can share the exact same
// visual treatment without duplicating the class list.
export function buttonClasses(variant: Variant = "primary", className = ""): string {
  return [
    "inline-flex items-center justify-center gap-1.5 rounded-md px-3.5 py-2 text-sm font-medium",
    "transition-colors duration-150 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2",
    "disabled:cursor-not-allowed disabled:opacity-50",
    variantClasses[variant],
    className,
  ].join(" ");
}

// The shared Button primitive — this file, and the rest of ./ui, is
// deliberately kept free of pgArchiMigrator-specific concepts (no "migration",
// "job", "phase" anywhere in this module) so it could be lifted into a
// standalone package for a future dataxtools.com tool with minimal
// changes, without forcing that decision today.
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = "primary", className = "", disabled, ...props },
  ref,
) {
  return <button ref={ref} disabled={disabled} className={buttonClasses(variant, className)} {...props} />;
});
