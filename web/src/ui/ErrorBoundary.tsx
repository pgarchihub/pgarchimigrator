import { Component, type ErrorInfo, type ReactNode } from "react";
import { Button } from "./Button";

interface Props {
  children: ReactNode;
}

interface State {
  error: Error | null;
}

// ErrorBoundary exists because of a real incident: a nil-vs-empty-slice
// bug in the Go backend (see internal/preview.Generate's fix) made a
// perfectly normal preview response crash the New Migration screen with
// "Cannot read properties of null" — and with NOTHING catching that
// error anywhere in the tree, React's default behavior is to unmount the
// ENTIRE app, leaving a blank white screen with no diagnostic info at
// all for the user (or for whoever they report the bug to).
//
// React error boundaries must currently be class components — there is
// no hook equivalent — so this one deliberately stays a thin, minimal
// class specifically for that purpose, not a style precedent for the
// rest of the (otherwise all-function-component) codebase.
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // eslint-disable-next-line no-console
    console.error("pgArchiMigrator UI crashed:", error, info.componentStack);
  }

  render() {
    if (this.state.error) {
      return (
        <div className="flex min-h-screen items-center justify-center bg-ink-50 px-4">
          <div className="w-full max-w-md rounded-lg border border-coral-200 bg-white p-6 text-center shadow-sm">
            <p className="mb-1 font-mono text-xs uppercase tracking-widest text-coral-500">Something went wrong</p>
            <h1 className="mb-2 text-lg font-medium text-ink-800">This screen hit an unexpected error</h1>
            <p className="mb-4 text-sm text-ink-500">
              Nothing you were working on was lost server-side — reloading usually recovers. If it keeps happening,
              the details below are worth including in a bug report.
            </p>
            <Button onClick={() => window.location.reload()} className="w-full">
              Reload
            </Button>
            <details className="mt-4 text-left">
              <summary className="cursor-pointer text-xs text-ink-400">Technical details</summary>
              <pre className="mt-2 overflow-x-auto rounded-md bg-ink-900 p-3 font-mono text-xs text-ink-50">
                {this.state.error.message}
              </pre>
            </details>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}
