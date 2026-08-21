# xfs-frag-exporter

A DaemonSet that exports **XFS free-space fragmentation** metrics for Prometheus,
to rank and identify degrading ("sick") nodes. It issues `XFS_IOC_GETFSMAP`
natively against every mounted XFS filesystem on a node — no `xfs_spaceman`, no
chroot, and no `privileged`/`CAP_SYS_ADMIN` for the metrics below.

Works on any Kubernetes/OpenShift node with XFS. See
[`xfs-frag-exporter-SPEC.md`](./xfs-frag-exporter-SPEC.md) for the design and the
research behind it, and [`docs/adr/`](./docs/adr) for the load-bearing decisions.

## What it does

Per XFS mount (labelled `node` + `mountpoint` + `device`) it exposes:

| Metric | Meaning |
|---|---|
| `xfs_frag_density_extents_per_gib` | free extents ÷ free GiB — the fragmentation signal |
| `xfs_free_extent_max_bytes` | largest contiguous free extent (ceilinged at agsize) |
| `xfs_free_extents` | number of free extents |
| `xfs_free_bytes` | total free space |
| `xfs_freesp_scrape_duration_seconds` | cost of the GETFSMAP walk |
| `xfs_freesp_scrape_success` | 1 if the last walk succeeded |

**Identifying a sick node:** fragmentation density scales with node age, so its
*level* is not enough. The discriminator is the **rate of change**:

```promql
# rank nodes fragmenting fastest (extents/GiB/day)
topk(10, deriv(xfs_frag_density_extents_per_gib[24h]) * 86400)
```

A node rising faster than ~3× the ~0.35/day aging baseline is fragmenting
abnormally for its age. Alert rules are in
[`deploy/prometheusrule.yaml`](./deploy/prometheusrule.yaml) — **thresholds there
are heuristic; calibrate them against your fleet.**

## Build & push

CI builds and pushes on every push to `main`: `.github/workflows/build.yml`
produces a multi-arch image at `ghcr.io/judexzhu/xfs-frag-exporter:latest`.
**One-time:** make the GHCR package public so nodes can pull without a secret
(repo → Packages → package → visibility).

Manual build:

```sh
REG=ghcr.io/judexzhu/xfs-frag-exporter
docker buildx build --platform linux/amd64,linux/arm64 -t $REG:latest --push .
```

## Deploy

```sh
oc apply -f deploy/namespace.yaml
oc apply -f deploy/rbac.yaml
oc apply -f deploy/daemonset.yaml
oc apply -f deploy/service.yaml
```

Verify:

```sh
oc -n xfs-frag-exporter rollout status ds/xfs-frag-exporter
oc -n xfs-frag-exporter exec ds/xfs-frag-exporter -- wget -qO- localhost:9101/metrics
```

## Deploy with Helm

```sh
helm install xfs-frag-exporter ./chart/xfs-frag-exporter
```

The chart creates the `xfs-frag-exporter` namespace (with the required privileged
Pod Security labels), a ServiceAccount, a DaemonSet, and a headless Service.
Common overrides:

```sh
# Prometheus Operator integration (ServiceMonitor + alert rules)
helm install xfs-frag-exporter ./chart/xfs-frag-exporter \
  --set serviceMonitor.enabled=true --set prometheusRule.enabled=true

# fall back to a privileged container if SELinux/permissions block host reads
helm install xfs-frag-exporter ./chart/xfs-frag-exporter --set privileged=true
```

All knobs are in [`chart/xfs-frag-exporter/values.yaml`](./chart/xfs-frag-exporter/values.yaml).

## Scraping

- **No Prometheus Operator / no UWM:** the pods carry `prometheus.io/scrape`
  annotations, or point a static Prometheus job at the headless Service
  (`xfs-frag-exporter.xfs-frag-exporter:9101`).
- **Prometheus Operator / OpenShift UWM:** also apply
  `deploy/servicemonitor.yaml` and `deploy/prometheusrule.yaml`. On OpenShift,
  enable UWM (`enableUserWorkload: true` in `cluster-monitoring-config`) and grant
  yourself `monitoring-edit` + `monitoring-rules-edit` in the namespace.

## Configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:9101` | HTTP listen address |
| `HOST_ROOT` | `/host` | where the node root fs is mounted in the pod |
| `SCRAPE_INTERVAL` | `300s` | GETFSMAP walk period (cost scales with fragmentation) |

## If least-privilege fails

The manifest runs non-root with all capabilities dropped and a read-only root fs.
If a node's SELinux policy or mount permissions stop it from opening the host
mounts (collection logs `permission denied` and `xfs_freesp_scrape_success` is 0),
relax the pod `securityContext` step by step: first `runAsUser: 0`, then if still
blocked, `privileged: true`. On OpenShift, granting `privileged` needs
cluster-admin (`oc adm policy add-scc-to-user privileged -z xfs-frag-exporter -n
xfs-frag-exporter`).

## Development

```sh
go test ./frag/                       # pure aggregation + the SPEC §6.3 fixture
GOOS=linux go build ./...             # the ioctl/collection code is Linux-only
```

## Not yet implemented (phase 1.5)

Free-extent size-distribution buckets, `xfs_avg_free_extent_bytes`, `statfs`
capacity/inode metrics, and `FSGEOMETRY` config metrics (`sparse`, `imaxpct`,
`agsize` — which would let `XFSLowContiguity` use a real `agsize` metric instead
of the hardcoded fleet constant).
