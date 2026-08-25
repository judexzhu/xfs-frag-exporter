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
| `xfs_free_extents_small` | free extents < 64 KiB (16 blocks); ÷ `xfs_free_extents` = the primary signal |
| `xfs_free_extents_tiny` | free extents < 8 KiB (2 blocks); can't fit even a sparse inode cluster |
| `xfs_free_extent_avg_blocks` | mean free extent in blocks; < 16 = danger (matches EKS `XFSSmallAverageClusterSize`) |
| `xfs_free_extent_avg_bytes` | same in bytes (kept for backward compat) |
| `xfs_frag_density_extents_per_gib` | free extents ÷ free GiB — reciprocal of avg extent size |
| `xfs_free_extent_max_bytes` | largest contiguous free extent (ceilinged at agsize) |
| `xfs_free_extents` | number of free extents |
| `xfs_free_bytes` | total free space |
| `xfs_agsize_bytes` | allocation group size from FSGEOMETRY (ceiling for max extent) |
| `xfs_sparse_inodes_enabled` | 1 if sparse=1, 0 if sparse=0 |
| `xfs_freesp_scrape_duration_seconds` | cost of the GETFSMAP walk |
| `xfs_freesp_scrape_success` | 1 if the last walk succeeded |

**Identifying a sick node:** the primary, field-validated signal is the **fraction
of free extents under 64 KiB** (`xfs_free_extents_small / xfs_free_extents`). On
the customer's live-incident nodes this was **0.95–0.98**; below ~16 blocks a free
extent can't satisfy an inode-cluster allocation, so XFS returns ENOSPC while `df`
still shows free GiB (Red Hat kernel tracking **RHEL-82924**) and `df -i` looks
healthy — free-space fragmentation, not inode exhaustion.

The **average free-extent size < 64 KiB** (equivalently frag density above
~16384 extents/GiB, the exact reciprocal; the AWS EKS `XFSSmallAverageClusterSize`
quantity) is a good **corroborating** signal, but it is a mean over *bytes*: a few
large free extents keep it high even when almost every extent is a tiny shard.
Live-incident node `ip-10-26-86-71` had 264 GB free across 652,406 extents — avg
~424 KiB, **above** the floor — yet 97.6% of extents were < 60 KiB. The count-based
fraction catches that; the average alone would have missed it.

```promql
# PRIMARY: >90% of free extents are tiny — ENOSPC-with-free-space risk
xfs_free_extents_small / xfs_free_extents > 0.90

# corroborating level, and rank the worst mounts
xfs_free_extent_avg_blocks < 16
topk(10, xfs_frag_density_extents_per_gib)
```

A `deriv()` rate query exists but is a **weak, informational secondary** — it is
false-positive prone (a rising slope over partial history flagged a 6-hour-old
node), so trust the *level* above, not the rate:

```promql
# informational only — noisy on young nodes
topk(10, deriv(xfs_frag_density_extents_per_gib[24h]) * 86400)
```

Alert rules are in [`deploy/prometheusrule.yaml`](./deploy/prometheusrule.yaml) —
**thresholds there are heuristic; calibrate them against your fleet.**

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
# OpenShift: hostPath needs an SCC that allows it (restricted-v2 doesn't)
oc adm policy add-scc-to-user hostmount-anyuid -z xfs-frag-exporter -n xfs-frag-exporter
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
# OpenShift: also bind an SCC that allows hostPath
helm install xfs-frag-exporter ./chart/xfs-frag-exporter --set rbac.sccBinding=true
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
- **OpenShift cluster monitoring (no UWM):** `--set clusterMonitoring.enabled=true`.
  The built-in platform `prometheus-k8s` scrapes the namespace directly, using the
  node-problem-detector pattern (KCS 7024333): a `prometheus-k8s` Role/RoleBinding,
  a NetworkPolicy allowing `openshift-monitoring` ingress, and the
  `openshift.io/cluster-monitoring: "true"` namespace label. No UWM stack needed.
- **Prometheus Operator / OpenShift UWM:** enable UWM
  (`enableUserWorkload: true` in `cluster-monitoring-config`), grant yourself
  `monitoring-edit` + `monitoring-rules-edit`, then `--set serviceMonitor.enabled=true`.

## Alerting

Recording rule + alerts live in [`deploy/prometheusrule.yaml`](./deploy/prometheusrule.yaml)
(chart: `--set prometheusRule.enabled=true`). With `clusterMonitoring.enabled=true`
the platform `prometheus-k8s` **evaluates** the `PrometheusRule` too (same
`openshift.io/cluster-monitoring` label that grants scrape) — no UWM needed.

| Alert | Fires on | Meaning |
|---|---|---|
| `XFSFreeSpaceFragmented` | `xfs_free_extents_small / xfs_free_extents > 0.90` for 30m | **primary:** most free extents < 64 KiB — ENOSPC-with-free-space risk (RHEL-82924); fired on every live-incident node (0.95–0.98) |
| `XFSSmallAverageExtent` | `xfs_free_extent_avg_blocks < 16` for 30m | corroborating: avg free extent below 16 blocks; matches EKS `XFSSmallAverageClusterSize`, but can miss a heavy tail |
| `XFSLowContiguity` | `xfs_free_extent_max_bytes / xfs_agsize_bytes < 0.10` for 30m | largest free extent collapsing — approaching dead |
| `XFSFragmentationRising` | `node:xfs_frag_density:rate1d > 1.05` for 2h | info / weak secondary — rate is false-positive prone; prefer the level above |
| `XFSSparseInodesDisabled` | `xfs_sparse_inodes_enabled == 0` | sparse=0 aggravates fragmentation ENOSPC; sparse=1 mitigates but is not immunity |
| `XFSExporterStale` | `xfs_freesp_scrape_success == 0` for 30m | collection health |

**Thresholds are heuristic — calibrate against your fleet.** `XFSFreeSpaceFragmented`
is the reliable trigger (`XFSSmallAverageExtent` corroborates but can miss the
heavy-tail case above); `XFSFragmentationRising` is kept only as informational — it
needs ~26h of history (`deriv[24h]` + `for:2h`) and on nodes younger than a day the
`deriv` fits a slope over partial data and swings wildly (seen -2.1…+1.2/day, and a
spurious pending alert on a 6-hour-old node), so expect noisy pending alerts.
`xfs_agsize_bytes` is now read from the filesystem via FSGEOMETRY — no manual tuning needed.

**Remediation is node replacement** — there is no live in-place fix (`xfs_fsr` needs
an unmount): cordon → drain → delete the machine (ROSA HCP via NodePool scaling,
ROSA Classic via MachineSet). Reduce the fragmentation *rate* preventively by
switching Fluent Bit to memory buffering where log loss is tolerable, or capping
`storage.max_chunks_up` / `storage.total_limit_size`, and by rotating long-lived nodes.

## Background / references

The failure this tool surfaces — XFS returning ENOSPC while `df`/`du` still show
free GiB on ROSA HCP / RHCOS — is **XFS free-space fragmentation** (Red Hat kernel
tracking **RHEL-82924**, confirmed expected behaviour, not a bug), not inode
exhaustion. The confirmed trigger is the **Fluent Bit / fluentd filesystem
buffer**: kernel tracing showed fluent-bit drives ~42× more XFS extent allocations
than any other process, so high-log-volume pods accelerate it and risk grows with
node age. This exporter is a working implementation of **RFE-9762** (expose the
detection metric via OpenShift Monitoring and enable node remediation). See
[`xfs-frag-exporter-SPEC.md`](./xfs-frag-exporter-SPEC.md) and
[`docs/adr/`](./docs/adr) for the full evidence trail.

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

## Reproducing & validating

[`deploy/examples/`](./deploy/examples) has a reproducer and a diagnostic script:

- `reproducer-node-var.yaml` — recreates RHEL-82924 on the node's real `/var` using
  the canonical **xfstests/xfs/076 pattern** (fill + gradual punch in 10 rounds +
  creat until ENOSPC). Demonstrates the full monitoring story: ratio climbs from
  healthy → alert fires → actual ENOSPC with free GiB remaining.
- `identify-culprit.sh` — ftrace-based script that traces XFS extent allocations per
  process and maps PID → pod via cgroup/CRI-O. Zero install, works on any RHCOS node.
  Also documents a bpftrace alternative via `quay.io/centos/centos:stream9`.

See [`deploy/examples/README.md`](./deploy/examples/README.md) for the full
walkthrough, decision rationale, and references.

## Development

```sh
go test ./frag/                       # pure aggregation + the SPEC §6.3 fixture
GOOS=linux go build ./...             # the ioctl/collection code is Linux-only
```

## Not yet implemented

Free-extent size-distribution buckets and `statfs` capacity metrics (inode counts
are already covered by node_exporter's `node_filesystem_files_free`).
