# pgarchimigrator (Helm chart)

Deploys pgArchiMigrator's REST API + web dashboard server (`pgarchimigrator
serve`) to Kubernetes as a **single-replica StatefulSet** with persistent
state storage. The dashboard is a React SPA served at `/app` (the domain
root redirects there); see Architecture Doc Section 5 for the design this
chart implements.

## Why a StatefulSet, and why only one replica

pgArchiMigrator's checkpoint store (`internal/state`) is SQLite-backed and
explicitly single-instance (Requirements Doc TR-13). Running more than one
replica — or losing the persistent volume — breaks the tool's core safety
guarantees:

- Two replicas could each believe they own a migration the other doesn't
  know about, double-creating or prematurely dropping PostgreSQL-side
  resources (replication slots, shadow tables) out from under an
  in-progress migration.
- `internal/reaper`'s orphan-cleanup and rollback-window sweeps rely
  entirely on what's in the state store — if it's lost, those PostgreSQL
  resources become permanently unrecoverable orphans, not just "the tool
  forgets about a job."

`replicas: 1` is therefore **hardcoded** in `templates/statefulset.yaml`,
not exposed as a values.yaml setting — this is a deliberate guardrail, not
an oversight.

## Prerequisites

- A reachable PostgreSQL instance with `wal_level=logical` if you intend
  to use the shadow-table strategy (Architecture Doc Section 3.2) — not
  required for the Direct DDL / Expand & Backfill strategies.
- A Kubernetes cluster with a default StorageClass (or set
  `persistence.storageClassName` explicitly).

## Quick start

```bash
# 1. Create the database credentials Secret out-of-band — never put a
#    real DSN in a values file that might get committed (TR-05).
kubectl create secret generic pgarchimigrator-db-credentials \
  --from-literal=database-url='postgresql://user:pass@host:5432/db?sslmode=require'

# 2. Install the chart, pointing it at that Secret.
helm install pgarchimigrator ./deploy/helm/pgarchimigrator \
  --set database.existingSecret.name=pgarchimigrator-db-credentials \
  --set image.tag=0.1.0

# 3. Follow the printed NOTES.txt for how to reach the dashboard.
```

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/pgarchihub/pgarchimigrator` | Image to deploy — build and push via the repo root `Dockerfile` |
| `image.tag` | `0.1.0` | Image tag |
| `service.type` | `ClusterIP` | `ClusterIP`, `NodePort`, or `LoadBalancer` |
| `service.port` | `8080` | Port the API/dashboard listens on |
| `persistence.enabled` | `true` | Whether to provision a PVC for `/data`. See the warning in `templates/statefulset.yaml` if you disable this. |
| `persistence.size` | `1Gi` | PVC size — the SQLite state DB and JSON Lines audit log are both small per-job, but audit logs grow unbounded over time; monitor and increase as needed |
| `persistence.storageClassName` | `""` (cluster default) | StorageClass for the PVC |
| `persistence.accessMode` | `ReadWriteOnce` | Fine for a single-replica StatefulSet |
| `database.existingSecret.name` | `""` | Name of a pre-existing Secret holding the DSN (**recommended**) |
| `database.existingSecret.key` | `database-url` | Key within that Secret |
| `database.inlineDSN` | `""` | Fallback for throwaway testing only — see the warning above |
| `actor` | `k8s-deployment` | `PGARCHIMIGRATOR_ACTOR` — identifies this deployment in the audit log (TR-07) |
| `autoSweep` | `true` | Runs `internal/reaper`'s periodic loop in the background (`pgarchimigrator serve --auto-sweep`) |
| `resources` | `100m/128Mi` requests, `500m/512Mi` limits | Adjust for your migration workload sizes |
| `serviceAccount.create` | `true` | Whether to create a dedicated ServiceAccount |

## Upgrading

```bash
helm upgrade pgarchimigrator ./deploy/helm/pgarchimigrator \
  --reuse-values \
  --set image.tag=<new-tag>
```

The StatefulSet's PVC is untouched by upgrades — state persists across
rolling updates.

## Uninstalling

```bash
helm uninstall pgarchimigrator
```

By default this does **not** delete the PVC (Kubernetes' standard
StatefulSet behavior) — the state store and audit log survive an
uninstall in case you're upgrading or reinstalling. Delete it explicitly
if you're truly done:

```bash
kubectl delete pvc data-pgarchimigrator-pgarchimigrator-0
```
