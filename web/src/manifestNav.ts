// manifestNav.ts is the single source of truth this project's own
// navigation is built from — Shell.tsx imports this array to render its
// own sidebar (mapping each iconKey to a real lucide-react component),
// and vite.config.ts's own manifestPlugin imports it at build time to
// emit dist/manifest.json's ui.nav[] field. Deliberately has zero
// React/JSX dependency (plain data only) so both consumers — one a
// React component, the other a Node build-time script — can import it
// without pulling in a JSX transform for the latter.
//
// See docs/ecosystem/COMPAT_BRIDGE_DESIGN.md's own sibling document
// (the reconstructed AC-PF-004 Dual-Shell Technical Specification,
// §6 Product Manifest Schema) for the ui.nav[]/ui.routes[] schema this
// mirrors. Per that document's own Reconstruction Notice, treat this
// manifest as directionally correct, not a verified-identical
// implementation of a since-lost original.
export interface NavItem {
  to: string;
  label: string;
  end?: boolean;
  // Matches lucide-react's own export names in kebab-case (e.g. "Layers"
  // -> "layers") — Shell.tsx's iconMap (below) is the single place that
  // translates this back to the real component; the manifest build step
  // emits this same string as-is.
  iconKey: string;
  minRole?: "operator" | "admin";
}

export const navItems: NavItem[] = [
  { to: "/", label: "Zero-Downtime Migration", end: true, iconKey: "layers" },
  // minRole: "operator" — matches GET /api/upgrades' own minimum role
  // (see internal/api/server.go's routes()); the page itself further
  // gates the "Start upgrade" action to admin, matching POST
  // /api/upgrades' own stricter minimum (a whole-database upgrade is a
  // substantially bigger action than a single-table migration).
  { to: "/upgrades", label: "Database Migration", minRole: "operator", iconKey: "database" },
  { to: "/users", label: "Users", minRole: "admin", iconKey: "users" },
];

// routes[] must be a superset of every path any nav[] entry or
// in-product link can navigate to (AC-PF-004 §6.1) — kept in sync BY
// HAND with App.tsx's own <Route path> declarations (there is no
// mechanical extraction from JSX route elements at build time; App.tsx
// is the source of truth for actual routing, this list mirrors it for
// the manifest only). /login and /setup are deliberately excluded —
// those are pre-auth screens, not this product's own content routes,
// and are not part of what a shell would navigate a signed-in user's
// embedded view to.
export const manifestRoutes: string[] = [
  "/",
  "/new",
  "/migrations/:id",
  "/upgrades",
  "/upgrades/new",
  "/upgrades/:id",
  "/users",
  "/help",
];
