import { type FormEvent, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useAuth } from "../lib/auth";
import { ApiError } from "../lib/api";
import { Button } from "../ui/Button";
import { TextField } from "../ui/TextField";
import { Card, CardBody } from "../ui/Card";
import { VersionBadge } from "../ui/VersionBadge";

// Setup is the first-run wizard shown instead of Login when
// GET /api/setup-required reports true (see auth.tsx's setupRequired) —
// removes the CLI (`pgarchimigrator auth create-admin`) as a REQUIRED step for
// getting started; it remains available for scripted/automated
// deployments, this is simply the friendlier path for a human doing a
// first-time install.
export default function Setup() {
  const { completeSetup } = useAuth();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);

    if (password !== confirmPassword) {
      setError("Passwords don't match.");
      return;
    }
    if (password.length < 8) {
      setError("Password must be at least 8 characters.");
      return;
    }

    setSubmitting(true);
    try {
      await completeSetup(email, password);
      navigate("/", { replace: true });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not complete setup — check the server is reachable.");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-ink-50 px-4">
      <div className="w-full max-w-sm">
        <div className="mb-6 text-center">
          <div className="mb-1 font-mono text-xs tracking-widest text-petrol-600">pgArchiMigrator</div>
          <h1 className="text-xl font-medium text-ink-800">Welcome — let's set up your admin account</h1>
          <p className="mt-2 text-sm text-ink-500">
            This is a one-time step. Once created, you'll be signed in and ready to go.
          </p>
        </div>
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
                autoComplete="new-password"
                required
                minLength={8}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
              <TextField
                label="Confirm password"
                type="password"
                autoComplete="new-password"
                required
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
              />
              {error && (
                <p role="alert" className="text-sm text-coral-500">
                  {error}
                </p>
              )}
              <Button type="submit" disabled={submitting} className="mt-1 w-full">
                {submitting ? "Creating account…" : "Create account & sign in"}
              </Button>
            </form>
          </CardBody>
        </Card>
        <VersionBadge />
      </div>
    </div>
  );
}
