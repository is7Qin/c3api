# c3api Helm chart

Deploys the [c3api](https://github.com/is7qin/c3api) gateway on Kubernetes.

The chart deploys **only the application**. PostgreSQL and Redis are **not
bundled** — you must point the chart at external instances.

## Requirements

- Kubernetes 1.25+ (uses `apps/v1`, `networking.k8s.io/v1`, `policy/v1`)
- Helm 3
- An external PostgreSQL and Redis reachable from the cluster

## Installing

```bash
helm install c3api ./deploy/helm \
  --set externalDatabase.dsn='postgres://user:pass@db:5432/c3api?sslmode=disable' \
  --set externalRedis.addr='redis:6379' \
  --set secrets.jwtSecret='<random-long-string>'
```

`externalDatabase.dsn`/`externalRedis.addr` and `secrets.jwtSecret` are
**required**; rendering fails with a `required` error if they are missing.

## Upgrading

```bash
helm upgrade c3api ./deploy/helm -f my-values.yaml
```

Changing `config.content` or `secrets.*` updates a checksum annotation and
triggers a rolling restart. The application creates its schema on first start
(fresh setup only — no migration path); back up external PostgreSQL before
upgrading.

## Uninstalling

```bash
helm uninstall c3api
```

This removes only the chart-managed resources; your external PostgreSQL and
Redis are untouched.

## Testing

```bash
helm test c3api
```

Runs a pod that `wget`s `/healthz` from the Service.

## Using an existing Secret / env-only mode

```bash
helm install c3api ./deploy/helm \
  --set secrets.create=false --set secrets.existingSecret=c3api-secrets \
  --set externalDatabase.existingSecret=c3api-secrets \
  --set externalRedis.existingSecret=c3api-secrets \
  --set config.inline=false
```

- `secrets.existingSecret` expects keys `auth-jwt-secret` (required) and
  `admin-token` (optional).
- `externalDatabase.existingSecret` expects key `db-dsn`.
- `externalRedis.existingSecret` expects key `redis-addr`.
- `config.inline=false` renders no ConfigMap and starts the container with
  `-config ""` (env-only); `C3API_SERVER_ADDR` is always injected so the process
  still listens on `app.port`.

## Values

| Key | Description | Default |
|---|---|---|
| `replicaCount` | Number of app replicas | `1` |
| `image.repository` | Image repository | `ghcr.io/is7qin/c3api` |
| `image.tag` | Image tag (pin a version in production) | `beta` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | Pull secrets for the image | `[]` |
| `nameOverride` | Override chart name | `""` |
| `fullnameOverride` | Override fully qualified name | `""` |
| `secrets.create` | Create a Secret from values | `true` |
| `secrets.existingSecret` | Existing Secret name (when `create=false`) | `""` |
| `secrets.jwtSecret` | JWT secret (required when `create=true`) | `""` |
| `secrets.adminToken` | Static admin token; empty = disabled (key not rendered) | `""` |
| `externalDatabase.dsn` | PostgreSQL DSN (or use existingSecret) | `""` |
| `externalDatabase.existingSecret` | Secret with key `db-dsn` | `""` |
| `externalRedis.addr` | Redis `host:port` (or use existingSecret) | `""` |
| `externalRedis.existingSecret` | Secret with key `redis-addr` | `""` |
| `config.inline` | Render config.toml ConfigMap; `false` = env-only | `true` |
| `config.content` | Free-form TOML; must keep `server.addr=":18080"` | `server = { addr = ":18080", time_zone = "" }`<br>`scheduler = { default_max_concurrency = 8 }` |
| `app.port` | Container listen port (`C3API_SERVER_ADDR`) | `18080` |
| `app.env` | Extra container env; empty values are skipped | `{GOGC:"", GOMEMLIMIT:""}` (both empty) |
| `app.resources` | Resource requests/limits | see `values.yaml` |
| `app.podAnnotations` | Pod annotations | `{}` |
| `app.nodeSelector` | Node selector | `{}` |
| `app.tolerations` | Tolerations | `[]` |
| `app.affinity` | Affinity | `{}` |
| `app.readOnlyRootFilesystem` | Read-only root filesystem | `true` |
| `app.tmpVolume` | Mount emptyDir at `/tmp` | `true` |
| `app.securityContext` | Non-root / dropped caps | see `values.yaml` |
| `app.probes.*` | startup/liveness/readiness probes on `/healthz` | enabled |
| `app.terminationGracePeriodSeconds` | Graceful shutdown budget | `60` |
| `app.strategy` | RollingUpdate `maxUnavailable`/`maxSurge` | `0` / `1` |
| `app.revisionHistoryLimit` | Old ReplicaSets kept | `3` |
| `app.minReadySeconds` | Seconds a pod must be ready before available | `0` |
| `service.type` | Service type | `ClusterIP` |
| `service.port` | Service port | `18080` |
| `ingress.enabled` | Render an Ingress | `false` |
| `ingress.className` | Ingress class | `""` |
| `ingress.annotations` | Ingress annotations | `{}` |
| `ingress.hosts` | Ingress hosts/paths | `c3api.example.com /` |
| `ingress.tls` | Ingress TLS | `[]` |
| `podDisruptionBudget.enabled` | Render a PDB (fails if `replicaCount=1`) | `false` |
| `podDisruptionBudget.minAvailable` | PDB minAvailable | `1` |
| `serviceAccount.create` | Create a ServiceAccount | `true` |
| `serviceAccount.annotations` | ServiceAccount annotations | `{}` |
| `serviceAccount.name` | ServiceAccount name | `""` |

## Notes

- **Ports:** the application default listen address is `:8080`; the chart's
  default `config.content` sets `server.addr=":18080"` and always injects
  `C3API_SERVER_ADDR=":<app.port>"` to match the Service and probes. If you
  override `app.port`, keep `config.content` consistent.
- **Probes:** all three probes use `/healthz`, which reports process liveness
  only (not DB/Redis). The app fail-fasts on dependencies at startup.
- **PodDisruptionBudget:** enabling it with `replicaCount=1` fails rendering
  (a PDB with `minAvailable=1` and a single replica blocks all voluntary
  evictions); use `replicaCount > 1`.
- **Secrets:** `secrets.*` and a literal `externalDatabase.dsn` are injected as
  plain env values (visible via `kubectl describe pod`). For production prefer
  `existingSecret`, and pin `image.tag` to an immutable version.
- **Admin token:** an empty `secrets.adminToken` disables the static admin token
  (the key is not rendered); `/api/admin` then accepts `platform_admin` JWTs only.
- **Config content:** `config.content` is emitted verbatim — Helm does NOT
  re-template values, so `{{` inside it is safe. Prefer `-f values.yaml` over
  `--set` for multi-line TOML.
- **Redis:** do not configure an `allkeys-lru` eviction policy; eviction only
  forces verification-code re-issue (harmless).
- **First user:** the first registered user becomes `platform_admin`. Sync
  pricing (`POST /api/admin/pricing/sync`) after the first boot.
