/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// build.outDir is picked up by internal/api's //go:embed directive —
// keep this in sync with cmd/pgarchimigrator's expectations if it ever moves.
export default defineConfig({
  plugins: [react()],
  // The SPA is served under /app (not the domain root — see
  // internal/api/server.go's webappFS doc comment for why), so every
  // asset reference the build emits (index.html's <script>/<link> tags,
  // and any dynamically-imported chunk URLs) must be prefixed with /app/
  // too, or the browser requests them against the root and gets a 404 —
  // which silently fails the whole app (blank white page, no visible
  // error) rather than a helpful one.
  base: "/app/",
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/setupTests.ts"],
    css: false,
    // Pinned to a non-UTC, non-trivial offset (Europe/Istanbul, UTC+3) —
    // deliberately NOT left to whatever the running machine's local
    // timezone happens to be. A real bug (Dashboard's date-range filter
    // silently using local time via setHours() instead of UTC) passed
    // every test in a UTC sandbox and only failed on a real Istanbul
    // machine, because a UTC+0 test runner made the local-vs-UTC bug
    // invisible by coincidence. Pinning this ensures every future
    // timezone-sensitive bug gets caught consistently regardless of
    // which machine or CI runner executes `npm test`.
    env: {
      TZ: "Europe/Istanbul",
    },
  },
  server: {
    // During `npm run dev`, proxy API calls to a real `pgarchimigrator serve`
    // instance running on :8080 — lets the SPA be developed against live
    // data without needing its own auth/CORS story.
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
});
