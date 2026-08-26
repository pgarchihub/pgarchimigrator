import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ErrorBoundary } from "./ErrorBoundary";

function Bomb(): never {
  throw new Error("simulated render crash");
}

describe("ErrorBoundary", () => {
  it("renders children normally when nothing throws", () => {
    render(
      <ErrorBoundary>
        <p>All good</p>
      </ErrorBoundary>,
    );
    expect(screen.getByText("All good")).toBeInTheDocument();
  });

  // This is the exact incident that motivated adding this component: a
  // real backend bug made a normal API response crash a page with no
  // error boundary anywhere in the tree, blanking the entire screen with
  // zero diagnostic information. This test proves the boundary actually
  // catches that class of error instead of letting it propagate and
  // unmount everything.
  it("catches a render-time error and shows a recovery screen instead of a blank one", () => {
    // React logs the caught error to the console by default; suppress
    // that expected noise so the test output stays readable.
    const consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});

    render(
      <ErrorBoundary>
        <Bomb />
      </ErrorBoundary>,
    );

    expect(screen.getByText(/something went wrong/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /reload/i })).toBeInTheDocument();
    expect(screen.getByText("simulated render crash")).toBeInTheDocument();

    consoleSpy.mockRestore();
  });

  it("the Reload button triggers a page reload", async () => {
    const consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const reloadSpy = vi.fn();
    // jsdom doesn't implement window.location.reload — stub it directly.
    Object.defineProperty(window, "location", {
      value: { ...window.location, reload: reloadSpy },
      writable: true,
    });

    const user = userEvent.setup();
    render(
      <ErrorBoundary>
        <Bomb />
      </ErrorBoundary>,
    );
    await user.click(screen.getByRole("button", { name: /reload/i }));

    expect(reloadSpy).toHaveBeenCalledTimes(1);
    consoleSpy.mockRestore();
  });
});
