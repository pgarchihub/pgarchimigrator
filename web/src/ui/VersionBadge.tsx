import { useEffect, useState } from "react";
import { api } from "../lib/api";

// VersionBadge shows which build is actually running — sourced from the
// backend (GET /api/version, itself sourced from internal/version.Version,
// injected at Docker build time — see the Dockerfile's VERSION build
// arg), never hardcoded in the frontend. A hardcoded frontend constant
// could drift from the backend binary actually serving it whenever only
// one side gets rebuilt; fetching it keeps this always accurate to
// what's really running, which is the whole point of showing it at all
// (e.g. for a support request: "what version are you on").
export function VersionBadge() {
  const [version, setVersion] = useState<string | null>(null);

  useEffect(() => {
    api
      .getVersion()
      .then((res) => setVersion(res.version))
      .catch(() => {
        // Purely cosmetic — silently omit rather than showing an error
        // for a detail this unimportant.
      });
  }, []);

  if (!version) return null;
  return <p className="mt-6 text-center font-mono text-xs text-ink-300">{version}</p>;
}
