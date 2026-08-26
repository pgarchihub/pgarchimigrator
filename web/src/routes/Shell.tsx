import { type ReactNode, useEffect, useRef } from "react";
import { NavLink, useLocation, useNavigate } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { Button } from "../ui/Button";
import { Badge } from "../ui/Badge";

const navItems = [
  { to: "/", label: "Migrations", end: true },
  { to: "/users", label: "Users", minRole: "admin" as const },
  { to: "/help", label: "Help" },
];

export function Shell({ children }: { children: ReactNode }) {
  const { user, logout, hasRole } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const mainRef = useRef<HTMLElement>(null);

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
  }, [location.pathname]);

  async function handleLogout() {
    await logout();
    navigate("/login", { replace: true });
  }

  return (
    <div className="min-h-screen bg-ink-50">
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
      <header className="border-b border-ink-200 bg-white">
        <div className="mx-auto flex max-w-6xl flex-wrap items-center justify-between gap-y-2 px-4 py-3 sm:px-6">
          <div className="flex items-center gap-4 sm:gap-8">
            <div className="flex items-center gap-1.5">
              <span className="font-mono text-sm font-semibold tracking-widest text-petrol-700">
                pgArchiMigrator
              </span>
              <span className="rounded bg-ink-100 px-1.5 py-0.5 font-mono text-[10px] font-medium text-ink-400">
                v1.0
              </span>
            </div>
            <nav aria-label="Main navigation" className="flex items-center gap-1">
              {navItems
                .filter((item) => !item.minRole || hasRole(item.minRole))
                .map((item) => (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.end}
                    className={({ isActive }) =>
                      [
                        "rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
                        isActive ? "bg-petrol-50 text-petrol-800" : "text-ink-500 hover:text-ink-800",
                      ].join(" ")
                    }
                  >
                    {item.label}
                  </NavLink>
                ))}
            </nav>
          </div>
          <div className="flex items-center gap-2 sm:gap-3">
            {user && (
              <>
                <Badge tone="petrol">{user.role}</Badge>
                {/* Hidden below sm: the role badge is enough identity on a
                    narrow phone header; the full email would either force
                    the header onto a second line or truncate unreadably. */}
                <span className="hidden text-sm text-ink-500 sm:inline">{user.email}</span>
              </>
            )}
            <Button variant="ghost" onClick={handleLogout}>
              Sign out
            </Button>
          </div>
        </div>
      </header>
      <main
        ref={mainRef}
        id="main-content"
        tabIndex={-1}
        className="mx-auto max-w-6xl px-4 py-8 sm:px-6 focus:outline-none"
      >
        {children}
      </main>
    </div>
  );
}
