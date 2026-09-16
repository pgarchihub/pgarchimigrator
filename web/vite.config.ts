/// <reference types="vitest/config" />
import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { navItems, manifestRoutes } from "./src/manifestNav";

// manifestPlugin emits dist/manifest.json at the end of every production
// build — the UI-facing manifest AC-PF-004 (the reconstructed Dual-Shell
// Technical Specification, docs/ecosystem/) §6 describes, served at
// GET /app/manifest.json once dist/ is deployed (base: "/app/" below).
// Deliberately generated from src/manifestNav.ts (the same data
// Shell.tsx renders its own sidebar from) rather than hand-maintained
// separately — one source of truth, no drift between what the sidebar
// shows and what this manifest advertises.
//
// This is deliberately the ONLY piece of AC-PF-004 implemented so far —
// see that document's own Reconstruction Notice and this project's own
// COMPAT_BRIDGE_DESIGN.md-adjacent decision log: a full Module
// Federation / mount() / ArchiContext integration was considered and
// deliberately deferred until a real second party (an actual
// ArchiConsole instance, or at least a concrete embedding test target)
// exists to validate the contract against — the same "don't build an
// interface only one side has ever used" principle applied to this
// project's ENGINE_MIGRATION_PLAN.md decision not to build a generic
// Engine interface before a second real engine existed. Publishing
// this manifest costs nothing and needs no counterpart to be useful
// (it's just data), unlike an actual mount()/postMessage contract.
function manifestPlugin(): Plugin {
  return {
    name: "pgarchimigrator-manifest",
    apply: "build",
    closeBundle() {
      const manifest = {
        productId: "pgarchimigrator",
        displayName: "pgArchiMigrator",
        ui: {
          nav: navItems.map((item) => ({
            label: item.label,
            route: item.to,
            icon: item.iconKey,
            ...(item.minRole ? { requiresRole: item.minRole } : {}),
          })),
          routes: manifestRoutes,
        },
      };
      writeFileSync(resolve(__dirname, "dist/manifest.json"), JSON.stringify(manifest, null, 2) + "\n");
    },
  };
}

// build.outDir is picked up by internal/api's //go:embed directive —
// keep this in sync with cmd/pgarchimigrator's expectations if it ever moves.
export default defineConfig({
  plugins: [react(), manifestPlugin()],
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
