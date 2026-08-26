import type { ReactNode } from "react";
import { Navigate, Route, Routes } from "react-router-dom";
import { AuthProvider, useAuth } from "./lib/auth";
import { Shell } from "./routes/Shell";
import Login from "./routes/Login";
import Setup from "./routes/Setup";
import Dashboard from "./routes/Dashboard";
import NewMigration from "./routes/NewMigration";
import MigrationDetail from "./routes/MigrationDetail";
import Users from "./routes/Users";
import Help from "./routes/Help";

function RequireAuth({ children }: { children: ReactNode }) {
  const { status } = useAuth();

  if (status === "loading") {
    return (
      <div className="flex min-h-screen items-center justify-center text-sm text-ink-400">
        Loading…
      </div>
    );
  }
  if (status === "anonymous") {
    return <Navigate to="/login" replace />;
  }
  return <>{children}</>;
}

function AppRoutes() {
  const { setupRequired, status } = useAuth();

  // Wait for BOTH the auth check and the setup-required check before
  // deciding where to route — without this, a fresh page load could
  // flash the login screen for a frame before redirecting to /setup (or
  // vice versa), which is exactly the kind of jarring first impression a
  // first-run wizard should avoid.
  if (setupRequired === null || status === "loading") {
    return (
      <div className="flex min-h-screen items-center justify-center text-sm text-ink-400">
        Loading…
      </div>
    );
  }

  // A fresh deployment with no admin account yet: every route leads to
  // the setup wizard, full stop — there is nothing else to show, and
  // nothing else CAN require auth yet since no account exists to
  // authenticate as.
  if (setupRequired) {
    return (
      <Routes>
        <Route path="/setup" element={<Setup />} />
        <Route path="*" element={<Navigate to="/setup" replace />} />
      </Routes>
    );
  }

  return (
    <Routes>
      {/* Setup is a ONE-TIME screen — once a deployment is bootstrapped,
          /setup has nothing left to do (the backend refuses a second
          setup attempt outright, see handleSetup), so send anyone who
          still has it bookmarked or types it manually to /login instead. */}
      <Route path="/setup" element={<Navigate to="/login" replace />} />
      <Route path="/login" element={<Login />} />
      <Route
        path="/*"
        element={
          <RequireAuth>
            <Shell>
              <Routes>
                <Route path="/" element={<Dashboard />} />
                <Route path="/new" element={<NewMigration />} />
                <Route path="/migrations/:id" element={<MigrationDetail />} />
                <Route path="/users" element={<Users />} />
                <Route path="/help" element={<Help />} />
                <Route path="*" element={<Navigate to="/" replace />} />
              </Routes>
            </Shell>
          </RequireAuth>
        }
      />
    </Routes>
  );
}

export default function App() {
  return (
    <AuthProvider>
      <AppRoutes />
    </AuthProvider>
  );
}
