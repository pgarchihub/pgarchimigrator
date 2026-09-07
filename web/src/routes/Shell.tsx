import { type ReactNode, useEffect, useRef, useState } from "react";
import { Link, NavLink, useLocation, useNavigate } from "react-router-dom";
import { ArrowRightLeft, ChevronDown, ChevronsLeft, ChevronsRight, CircleHelp, Database, Layers, LogOut, Users as UsersIcon } from "lucide-react";
import { useAuth } from "../lib/auth";
import { Badge } from "../ui/Badge";

// SIDEBAR_COLLAPSED_KEY persists purely as a per-browser UI preference
// (not app data) — a plain localStorage read/write is the right tool
// here, unlike the artifact-sandbox restriction on browser storage
// this project's own instructions mention elsewhere: that restriction
// is about Claude-generated Artifacts running in an iframe, not this
// actual production web app.
const SIDEBAR_COLLAPSED_KEY = "pgarchimigrator_sidebar_collapsed";

// Help intentionally lives in the header, not this list — it's a
// constant, always-available reference (see NavItem's own role split:
// header = "always there regardless of what you're doing," sidebar =
// "which of this app's own tools am I using right now").
const navItems = [
  { to: "/", label: "Zero-Downtime Migration", end: true, icon: Layers },
  // minRole: "operator" — matches GET /api/upgrades' own minimum role
  // (see internal/api/server.go's routes()); the page itself further
  // gates the "Start upgrade" action to admin, matching POST
  // /api/upgrades' own stricter minimum (a whole-database upgrade is a
  // substantially bigger action than a single-table migration).
  //
  // "Database Migration" (not "Data Synchronization") is the displayed
  // label deliberately — "Synchronization" risks implying an ongoing,
  // continuous sync (like a CDC/ETL tool), when this is a one-time,
  // terminal move (Introspecting → ... → Ready, no further syncing
  // after that); "Database Migration" also keeps naming consistent
  // with "Zero-Downtime Migration" above (same "Migration" vocabulary,
  // differentiated by scope: one table in place vs. a whole database
  // to a different instance). The underlying route (/upgrades) and
  // backend paths (/api/upgrades) are untouched — this is a display
  // label only.
  { to: "/upgrades", label: "Database Migration", minRole: "operator" as const, icon: Database },
  { to: "/users", label: "Users", minRole: "admin" as const, icon: UsersIcon },
];

export function Shell({ children }: { children: ReactNode }) {
  const { user, logout, hasRole } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const mainRef = useRef<HTMLElement>(null);

  const [collapsed, setCollapsed] = useState(() => {
    try {
      return window.localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "true";
    } catch {
      // Private browsing / storage disabled — default to expanded
      // rather than letting a storage error break the whole shell.
      return false;
    }
  });

  useEffect(() => {
    try {
      window.localStorage.setItem(SIDEBAR_COLLAPSED_KEY, String(collapsed));
    } catch {
      // Nothing to do — this is a nice-to-have preference, not
      // something worth surfacing an error for.
    }
  }, [collapsed]);

  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const userMenuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!userMenuOpen) return;
    function handleClickOutside(e: MouseEvent) {
      if (userMenuRef.current && !userMenuRef.current.contains(e.target as Node)) {
        setUserMenuOpen(false);
      }
    }
    function handleEscape(e: KeyboardEvent) {
      if (e.key === "Escape") setUserMenuOpen(false);
    }
    document.addEventListener("mousedown", handleClickOutside);
    document.addEventListener("keydown", handleEscape);
    return () => {
      document.removeEventListener("mousedown", handleClickOutside);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [userMenuOpen]);

  // SPA route changes don't trigger a real page load, so a screen reader
  // never gets its usual "new page" announcement, and keyboard focus just
  // stays wherever it was — often on a link/button that's no longer even
  // on the page. Moving focus to <main> on every navigation (a tabIndex=-1
  // landmark, so it's programmatically focusable without joining the
  // normal Tab order) is the standard WAI-ARIA Authoring Practices fix:
  // it both gives assistive tech a natural place to announce from and
  // puts sighted keyboard users' focus somewhere sensible again.
  useEffect(() => {
    mainRef.current?.focus();
    setUserMenuOpen(false);
  }, [location.pathname]);

  async function handleLogout() {
    await logout();
    navigate("/login", { replace: true });
  }

  // "Switch user" and "Sign out" both end up calling the same server
  // action (there's no multi-account/session-stacking concept in this
  // app's own auth model) — they're offered as two separate menu items
  // because they answer two different intents someone might have
  // ("I want to hand this session to a different person" vs. "I'm
  // done"), even though the underlying mechanism is identical.
  async function handleSwitchUser() {
    await handleLogout();
  }

  const visibleNavItems = navItems.filter((item) => !item.minRole || hasRole(item.minRole));

  return (
    // h-screen + overflow-hidden (not min-h-screen) is the actual fix
    // for two related complaints at once: min-h-screen only sets a
    // FLOOR on height, so a tall page's content grows the whole page
    // past the viewport and the BROWSER itself scrolls — meaning
    // header/sidebar scroll away too, and main's own overflow-y-auto
    // below never even gets a chance to kick in (its parent already
    // has room to grow instead of clipping). Fixing the outer height
    // to exactly the viewport forces the flex children to divide that
    // FIXED space instead, which is what makes header/sidebar
    // genuinely stay put while only <main> scrolls internally.
    <div className="flex h-screen flex-col overflow-hidden bg-ink-50">
      {/* Visually hidden until focused (Tab from the very top of the
          page) — lets a keyboard user jump straight past the header's
          logo/nav/user-menu instead of tabbing through all of it on
          every single page. */}
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:fixed focus:left-4 focus:top-4 focus:z-50 focus:rounded-md focus:bg-petrol-700 focus:px-4 focus:py-2 focus:text-sm focus:font-medium focus:text-white"
      >
        Skip to main content
      </a>

      <header className="flex h-14 shrink-0 items-center justify-between border-b border-ink-200 bg-white px-4 sm:px-6">
        <Link to="/" className="flex items-center gap-1.5 rounded-md focus:outline-none focus-visible:ring-2 focus-visible:ring-petrol-500">
          <span className="font-mono text-sm font-semibold tracking-widest text-petrol-700">pgArchiMigrator</span>
          <span className="rounded bg-ink-100 px-1.5 py-0.5 font-mono text-[10px] font-medium text-ink-400">v2.0</span>
        </Link>

        <div className="flex items-center gap-1 sm:gap-2">
          <NavLink
            to="/help"
            title="Help"
            className={({ isActive }) =>
              [
                "flex h-9 w-9 items-center justify-center rounded-md transition-colors",
                isActive ? "bg-petrol-50 text-petrol-700" : "text-ink-400 hover:bg-ink-50 hover:text-ink-700",
              ].join(" ")
            }
          >
            <CircleHelp className="h-5 w-5" aria-hidden="true" />
            <span className="sr-only">Help</span>
          </NavLink>

          {user && (
            <div className="relative" ref={userMenuRef}>
              <button
                type="button"
                onClick={() => setUserMenuOpen((v) => !v)}
                aria-haspopup="menu"
                aria-expanded={userMenuOpen}
                className="flex items-center gap-2 rounded-md py-1.5 pl-2 pr-1.5 text-sm hover:bg-ink-50"
              >
                <Badge tone="petrol">{user.role}</Badge>
                {/* Hidden below sm: the role badge is enough identity on a
                    narrow phone header; the full email would either force
                    the header onto a second line or truncate unreadably. */}
                <span className="hidden max-w-[14rem] truncate text-ink-600 sm:inline">{user.email}</span>
                <ChevronDown
                  className={`h-4 w-4 text-ink-400 transition-transform ${userMenuOpen ? "rotate-180" : ""}`}
                  aria-hidden="true"
                />
              </button>

              {userMenuOpen && (
                <div
                  role="menu"
                  className="absolute right-0 top-full z-20 mt-1 w-56 overflow-hidden rounded-lg border border-ink-200 bg-white py-1 shadow-lg"
                >
                  <div className="border-b border-ink-100 px-3 py-2 sm:hidden">
                    <p className="truncate text-sm text-ink-700">{user.email}</p>
                  </div>
                  <button
                    role="menuitem"
                    type="button"
                    onClick={handleSwitchUser}
                    className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm text-ink-600 hover:bg-ink-50"
                  >
                    <ArrowRightLeft className="h-4 w-4 text-ink-400" aria-hidden="true" />
                    Switch user
                  </button>
                  <button
                    role="menuitem"
                    type="button"
                    onClick={handleLogout}
                    className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm text-coral-600 hover:bg-coral-50"
                  >
                    <LogOut className="h-4 w-4" aria-hidden="true" />
                    Sign out
                  </button>
                </div>
              )}
            </div>
          )}
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        <nav
          aria-label="Main navigation"
          className={[
            "flex shrink-0 flex-col overflow-y-auto border-r border-ink-200 bg-white transition-[width] duration-150",
            collapsed ? "w-14" : "w-60",
          ].join(" ")}
        >
          <div className="border-b border-ink-100 p-2">
            <button
              type="button"
              onClick={() => setCollapsed((v) => !v)}
              title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
              className={[
                "flex w-full items-center gap-3 rounded-md px-2.5 py-2 text-sm text-ink-400 hover:bg-ink-50 hover:text-ink-700",
                collapsed ? "justify-center" : "",
              ].join(" ")}
            >
              {collapsed ? (
                <ChevronsRight className="h-[18px] w-[18px] shrink-0" aria-hidden="true" />
              ) : (
                <ChevronsLeft className="h-[18px] w-[18px] shrink-0" aria-hidden="true" />
              )}
              {!collapsed && <span>Collapse</span>}
              <span className="sr-only">{collapsed ? "Expand sidebar" : "Collapse sidebar"}</span>
            </button>
          </div>

          <ul className="flex flex-1 flex-col gap-0.5 p-2">
            {visibleNavItems.map((item) => {
              const Icon = item.icon;
              return (
                <li key={item.to}>
                  <NavLink
                    to={item.to}
                    end={item.end}
                    title={collapsed ? item.label : undefined}
                    className={({ isActive }) =>
                      [
                        "flex items-center gap-3 rounded-md px-2.5 py-2 text-sm font-medium transition-colors",
                        collapsed ? "justify-center" : "",
                        isActive ? "bg-petrol-50 text-petrol-800" : "text-ink-500 hover:bg-ink-50 hover:text-ink-800",
                      ].join(" ")
                    }
                  >
                    <Icon className="h-[18px] w-[18px] shrink-0" aria-hidden="true" />
                    {!collapsed && <span className="truncate">{item.label}</span>}
                    {collapsed && <span className="sr-only">{item.label}</span>}
                  </NavLink>
                </li>
              );
            })}
          </ul>
        </nav>

        <main
          ref={mainRef}
          id="main-content"
          tabIndex={-1}
          className="min-w-0 flex-1 overflow-y-auto px-4 py-8 focus:outline-none sm:px-6"
        >
          <div className="mx-auto max-w-6xl">{children}</div>
        </main>
      </div>
    </div>
  );
}
