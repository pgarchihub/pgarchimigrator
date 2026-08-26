import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// React 18 requires this flag to be explicitly set in test environments
// that aren't the full Jest/jsdom preset (Vitest's jsdom environment
// doesn't set it automatically) — without it, React prints
// "The current testing environment is not configured to support act(...)"
// on every act() call even though the tests themselves still pass.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).IS_REACT_ACT_ENVIRONMENT = true;

// @testing-library/react's automatic cleanup relies on detecting a global
// test framework's afterEach hook. This project deliberately does NOT
// enable Vitest's `globals: true` (to avoid needing extra tsconfig `types`
// entries — see vite.config.ts), so that auto-detection doesn't fire and
// DOM from one test leaks into the next unless cleanup() is called
// explicitly here, once, for every test file.
afterEach(() => {
  cleanup();
});
