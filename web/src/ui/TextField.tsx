import { type InputHTMLAttributes, forwardRef } from "react";

interface TextFieldProps extends InputHTMLAttributes<HTMLInputElement> {
  label: string;
  error?: string;
}

export const TextField = forwardRef<HTMLInputElement, TextFieldProps>(function TextField(
  { label, error, id, className = "", ...props },
  ref,
) {
  const inputId = id ?? `field-${label.toLowerCase().replace(/\s+/g, "-")}`;
  return (
    // The error message below is a SIBLING of <label>, not a descendant of
    // it — deliberately. A <label> that implicitly wraps other visible
    // text (like an error message) has that text folded into the field's
    // ACCESSIBLE NAME by browsers, not just announced as a description —
    // so "Column" would become something like "Column, This field can't
    // be empty" as the announced NAME, duplicating what aria-describedby
    // already announces separately as a description. aria-describedby
    // only needs matching ids, not DOM nesting, so moving the error
    // outside costs nothing functionally and fixes the duplication.
    <div className="flex flex-col gap-1.5">
      <label htmlFor={inputId} className="flex flex-col gap-1.5">
        <span className="text-sm font-medium text-ink-700">
          {label}
          {props.required && (
            <>
              {/* The asterisk is aria-hidden because native `required`
                  below already makes screen readers announce "required" —
                  without hiding it, some would additionally read the
                  glyph itself ("asterisk") as visible text, which is just
                  noise. Sighted users still get the familiar visual cue. */}
              <span aria-hidden="true" className="ml-0.5 text-coral-500">
                *
              </span>
              <span className="sr-only"> (required)</span>
            </>
          )}
        </span>
        <input
          ref={ref}
          id={inputId}
          className={[
            "rounded-md border px-3 py-2 text-sm text-ink-800 placeholder:text-ink-300",
            "focus:outline-none focus:ring-2 focus:ring-petrol-500 focus:border-petrol-500",
            error ? "border-coral-400" : "border-ink-200",
            className,
          ].join(" ")}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? `${inputId}-error` : undefined}
          {...props}
        />
      </label>
      {error && (
        <span id={`${inputId}-error`} className="text-xs text-coral-500">
          {error}
        </span>
      )}
    </div>
  );
});
