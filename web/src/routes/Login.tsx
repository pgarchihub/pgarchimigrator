import { type FormEvent, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { ApiError } from "../lib/api";
import { Button } from "../ui/Button";
import { TextField } from "../ui/TextField";
import { Card, CardBody } from "../ui/Card";
import { VersionBadge } from "../ui/VersionBadge";

export default function Login() {
  const { login, sessionExpired, clearSessionExpired } = useAuth();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    clearSessionExpired();
    setSubmitting(true);
    try {
      await login(email, password);
      navigate("/", { replace: true });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not sign in — check the server is reachable.");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-ink-50 px-4">
      <div className="w-full max-w-sm">
        <div className="mb-6 text-center">
          <div className="mb-1 font-mono text-xs tracking-widest text-petrol-600">pgArchiMigrator</div>
          <h1 className="text-xl font-medium text-ink-800">Sign in to Migrator</h1>
        </div>
        {sessionExpired && (
          <div role="alert" className="mb-4 rounded-md bg-amber-50 px-3 py-2 text-sm text-amber-600">
            Your session has expired. Please sign in again.
          </div>
        )}
        <Card>
          <CardBody>
            <form onSubmit={handleSubmit} className="flex flex-col gap-4">
              <TextField
                label="Email"
                type="email"
                autoComplete="username"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
              <TextField
                label="Password"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
              {error && (
                <p role="alert" className="text-sm text-coral-500">
                  {error}
                </p>
              )}
              <Button type="submit" disabled={submitting} className="mt-1 w-full">
                {submitting ? "Signing in…" : "Sign in"}
              </Button>
            </form>
          </CardBody>
        </Card>
        <VersionBadge />
      </div>
    </div>
  );
}
