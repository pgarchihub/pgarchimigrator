import { type FormEvent, useCallback, useEffect, useState } from "react";
import { api, ApiError } from "../lib/api";
import type { ManagedUser, Role } from "../lib/types";
import { useAuth } from "../lib/auth";
import { Button } from "../ui/Button";
import { Card, CardBody, CardHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { TextField } from "../ui/TextField";

const roleTone: Record<Role, "petrol" | "amber" | "neutral"> = {
  admin: "petrol",
  operator: "amber",
  viewer: "neutral",
};

export default function Users() {
  const { user: currentUser } = useAuth();
  const [users, setUsers] = useState<ManagedUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [updatingRoleId, setUpdatingRoleId] = useState<string | null>(null);

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("viewer");
  const [formError, setFormError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const load = useCallback(async () => {
    try {
      const result = await api.listUsers();
      setUsers(result);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not load users.");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  async function handleCreate(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    setSubmitting(true);
    try {
      await api.createUser(email, password, role);
      setEmail("");
      setPassword("");
      setRole("viewer");
      await load();
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Could not create user.");
    } finally {
      setSubmitting(false);
    }
  }

  async function handleDelete(id: string, userEmail: string) {
    if (!window.confirm(`Remove ${userEmail}? They will lose access immediately.`)) return;
    try {
      await api.deleteUser(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not remove this user.");
    }
  }

  // Changing role is a direct save-on-change, not a "form with a submit
  // button" — this is a single-field, low-friction action an admin does
  // one row at a time, and adding a separate save step would just be an
  // extra click with no real benefit (unlike, say, the Add User form
  // below, which has several fields worth reviewing together first).
  async function handleRoleChange(id: string, newRole: Role) {
    setUpdatingRoleId(id);
    setError(null);
    try {
      await api.updateUserRole(id, newRole);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not update this user's role.");
    } finally {
      setUpdatingRoleId(null);
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-lg font-medium text-ink-800">Users</h1>
        <p className="text-sm text-ink-500">Everyone in your organization, and what they can do.</p>
      </div>

      {error && (
        <Card className="border-coral-200 bg-coral-50">
          <div className="px-5 py-4 text-sm text-coral-600">{error}</div>
        </Card>
      )}

      {loading && !users && <p className="text-sm text-ink-500">Loading users…</p>}

      {users && (
        <Card className="overflow-hidden">
          <div className="overflow-x-auto">
            <table aria-label="Users" className="w-full text-sm">
              <thead>
                <tr className="border-b border-ink-100 bg-ink-50 text-left text-xs uppercase tracking-wide text-ink-400">
                  <th className="px-5 py-3 font-medium">Email</th>
                  <th className="px-5 py-3 font-medium">Role</th>
                  <th className="px-5 py-3 font-medium"></th>
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.id} className="border-b border-ink-50 last:border-0">
                    <td className="whitespace-nowrap px-5 py-3 text-ink-800">
                      {u.email}
                      {u.id === currentUser?.id && <span className="ml-2 text-xs text-ink-400">(you)</span>}
                    </td>
                    <td className="whitespace-nowrap px-5 py-3">
                      {u.id === currentUser?.id ? (
                        // Backend refuses a self-role-change (see
                        // handleUpdateUserRole's doc comment: an
                        // accidental self-demotion could lock out the
                        // only admin) — shown read-only here to match,
                        // rather than offering a control that would
                        // always fail.
                        <Badge tone={roleTone[u.role]}>{u.role}</Badge>
                      ) : (
                        <select
                          value={u.role}
                          onChange={(e) => handleRoleChange(u.id, e.target.value as Role)}
                          disabled={updatingRoleId === u.id}
                          aria-label={`Role for ${u.email}`}
                          className="rounded-md border border-ink-200 px-2 py-1 text-sm text-ink-800 focus:outline-none focus:ring-2 focus:ring-petrol-500 focus:border-petrol-500 disabled:opacity-50"
                        >
                          <option value="viewer">viewer</option>
                          <option value="operator">operator</option>
                          <option value="admin">admin</option>
                        </select>
                      )}
                    </td>
                    <td className="whitespace-nowrap px-5 py-3 text-right">
                      {u.id !== currentUser?.id && (
                        <Button
                          variant="ghost"
                          onClick={() => handleDelete(u.id, u.email)}
                          aria-label={`Remove ${u.email}`}
                        >
                          Remove
                        </Button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      <Card>
        <CardHeader>
          <span className="text-sm font-medium text-ink-700">Add a user</span>
        </CardHeader>
        <CardBody>
          <form onSubmit={handleCreate} className="grid grid-cols-1 gap-4 sm:grid-cols-3 sm:items-end">
            <TextField
              label="Email"
              type="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
            <TextField
              label="Password"
              type="password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            <label className="flex flex-col gap-1.5">
              <span className="text-sm font-medium text-ink-700">Role</span>
              <select
                value={role}
                onChange={(e) => setRole(e.target.value as Role)}
                className="rounded-md border border-ink-200 px-3 py-2 text-sm text-ink-800 focus:outline-none focus:ring-2 focus:ring-petrol-500 focus:border-petrol-500"
              >
                <option value="viewer">viewer</option>
                <option value="operator">operator</option>
                <option value="admin">admin</option>
              </select>
            </label>
            <div className="sm:col-span-3">
              {formError && <p className="mb-3 text-sm text-coral-500">{formError}</p>}
              <Button type="submit" disabled={submitting}>
                {submitting ? "Adding…" : "Add user"}
              </Button>
            </div>
          </form>
        </CardBody>
      </Card>
    </div>
  );
}
