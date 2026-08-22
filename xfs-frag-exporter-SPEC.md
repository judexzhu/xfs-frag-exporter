# Build Spec v2 — XFS Free-Space Fragmentation Exporter

> Corrected plan. Supersedes the v1 AI-generated spec after first-party research
> and a real-node measurement. v1's evidence (the 3-node table) is retained; its
> conclusions, failure premise, collection mechanism, and alert design are
> revised. See `docs/adr/0001` and `docs/adr/0002` for the load-bearing decisions.

## 0. What changed from v1, and why

| v1 claim | Verdict | Correction |
|---|---|---|
| ENOSPC-despite-free-space from free-space fragmentation is the target failure | **Overstated for RHEL 9 / RHCOS** | `sparse=1` is the default on every ROSA node (confirmed by `xfs_info /var`). Sparse inodes drop the inode-chunk floor from 8 contiguous blocks to a single 4 KiB block, so the fragmentation-driven inode-ENOSPC path is designed out. Red Hat KCS 7110315: "no indication of this issue happening in RHEL 9." **Reframe the tool from *predict ENOSPC* to *identify fragmented/degrading nodes* (observability).** |
| `privileged: true` + `CAP_SYS_ADMIN` required | **Refuted** | `XFS_IOC_GETFSMAP` has no capability gate for free space; unprivileged callers walk the free-space btree (`bnobt`). Your nodes have `rmapbt=0`, so the privileged rmap path isn't even available. Free-space reads need **no `CAP_SYS_ADMIN`, no `privileged`** — only a hostPath mount. |
| chroot to host `xfs_spaceman -c 'freesp -s'` | **Wrong call** | `freesp` is just GETFSMAP + userspace bucketing, and it never even outputs the largest extent. Issue GETFSMAP **natively from Go**. Static binary depends only on the stable-since-4.12 kernel ABI — no chroot, no host binary/libs, no version-mismatch. |
| `baseline_ratio` recording rule (`0.35 × uptime`) | **Broken twice** | `0.35` is fit to 3 nodes; and it divides by node **uptime**, which resets on reboot while filesystem age does not. Dropped entirely. |
| `xfs_extent_alloc_*` / `xfs_extent_free_*` counters | **Redundant** | `node_exporter` already exports these as `node_xfs_extent_allocation_*`. Don't duplicate. |
| ROSA-HCP-specific (MachineConfig webhook, HyperShift ignition, autoRepair) | **Deferred** | Tool is generic k8s/OCP-on-XFS. ROSA specifics → **stage 2**. All ROSA constraints re-verified (see §7). |

Research sources: Red Hat KCS 7110315; `mkfs.xfs(8)` (sparse default since xfsprogs 4.16);
kernel `fs/xfs/xfs_fsmap.c` (no cap gate); `ioctl_getfsmap(2)` (ABI since 4.12);
AWS EKS NMA docs (`XFSSmallAverageClusterSize` = Event, repair None).

## 1. The need

Under container churn, an XFS filesystem's free space fragments: the same free
bytes get split across more and smaller extents over time. On modern RHCOS
(`sparse=1`) this no longer causes the classic `creat()`-ENOSPC failure, but it
is still a real signal of a **degrading node** — the allocator works harder,
large-contiguous allocations get scarcer, and it is a leading indicator that
tracks node age and workload churn. The kernel exposes the free-extent **size
distribution** only via `XFS_IOC_GETFSMAP` (no `/proc` or `/sys` equivalent), so
nothing in `node_exporter`, kubelet, or `DiskPressure` can see it today.

**Goal: give operators a per-node, per-mount fragmentation signal to rank and
identify "sick" nodes** — not to predict ENOSPC (which `sparse=1` prevents).

### Evidence (v1, retained — real measurements, 4K bsize)

| Node | Age | Used | Free extents | Free blocks | Avg free extent | MAX_EXT |
|---|---|---|---|---|---|---|
| A | 3.78 d | 5% | 358 | 74,739,205 | 208,769 blk | 348,288 |
| C | 8.30 d | 59% | 449 | 33,074,036 | 73,662 blk | 348,288 |
| B | 38.70 d | 15% | 3,094 | 67,924,999 | 21,954 blk | 348,288 |

**(a) `frag_density` and average free extent size are the same signal.** For a
fixed block size, `frag_density = free_extents / free_GiB = (2^30 / bsize) /
avg_free_extent_blocks` — at 4K, `density = 262144 / avg_blocks` (check:
262144 / 208769 = 1.2557 = node A). They are **exact reciprocals**: identical
information, identical node ranking. Choosing density over avg buys intuitiveness
(higher = worse, per-GiB-free), **not** a removed confound. (v1 claimed density
cancels a utilisation confound that avg has — mathematically false; corrected.)

**(b) The real flaw is alerting on an absolute *level*.** Any static threshold on
density (or avg) fires on old-but-normal nodes, because the level scales with age
— density ÷ age holds ~0.31–0.43 across a 10× age range:

| Node | density | density ÷ age |
|---|---|---|
| A | 1.26 | 0.33 |
| C | 3.56 | 0.43 |
| B | 11.94 | 0.31 |

So the discriminator for a *sick* (not merely old) node is the **rate of change**
of density, not its level — this is also the true flaw in EKS NMA's
`XFSSmallAverageClusterSize` (it alarms on an absolute average). Aging baseline
≈ 0.35 extents/GiB/day, but that is **3 cross-sectional points (different nodes,
not one node over time) — do NOT treat 0.35 or any derived threshold as
validated.** Prometheus `deriv` of the density gauge is the defensible signal;
thresholds need fleet calibration.

**(c) MAX_EXT is ceilinged at agsize.** A free extent cannot span an allocation
group. The measured node has `agsize=350336 blks` (~1.33 GiB); all three nodes
reported `MAX_EXT=348288` — i.e. ~one nearly-empty AG. **MAX_EXT ≈ agsize is
healthy; it is only meaningful when it drops far below agsize.** Absolute MAX_EXT
thresholds (v1's `262144` / `16384`) are therefore misleading without agsize.

## 2. What to build

A **generic Kubernetes/OpenShift DaemonSet** (any node with XFS mounts) that,
per interval, issues `XFS_IOC_GETFSMAP` natively against each discovered XFS
mount, aggregates the free-extent geometry, and serves Prometheus metrics.

- **Native Go**, static CGO-free binary issuing the raw ioctl. Base image
  `scratch`. Pushed to the operator's Quay.io repo.
- **No chroot, no `xfs_spaceman`, no `CAP_SYS_ADMIN`, no `privileged`** for the
  MVP path. Mount host `/` read-only at `/host` (`mountPropagation: HostToContainer`);
  discover XFS mounts from `/host/proc/1/mountinfo`; open each mountpoint
  `O_RDONLY` and ioctl.
- **Least-privilege** deploy (§7). If a cluster's SELinux/SCC blocks the hostPath
  open, fall back to `privileged: true` and harden later.
- The exporter **writes nothing to any host filesystem** (it would add to the
  churn it measures). Scratch is memory-only.
- Collection interval **300 s** default (the GETFSMAP walk cost scales with
  fragmentation and takes the per-AG AGF lock in batches). Export scrape duration
  so cost is observable; align any ServiceMonitor interval to match.
- Serve metrics from an in-memory snapshot so a slow walk never blocks a scrape.

## 3. Metrics

Every series labelled `node`, `mountpoint` and `device` (`node` from the
downward-API `NODE_NAME`, so the sick node is identifiable). Emitted per
discovered XFS mount.

### MVP — the sick-node signal (all from one GETFSMAP walk)

| Metric | Type | Note |
|---|---|---|
| `xfs_frag_density_extents_per_gib` | gauge | **primary.** `free_extents / (free_bytes / 2^30)`. Fixture → 1.2557 |
| `xfs_free_extent_max_bytes` | gauge | **primary.** largest contiguous free extent. Interpret vs agsize. Fixture → 1,426,587,648 (348,288 blk) |
| `xfs_free_extents` | gauge | count of free extents |
| `xfs_free_bytes` | gauge | Σ free-extent bytes (density denominator; cross-check vs `statfs`) |
| `xfs_freesp_scrape_duration_seconds` | gauge | cost of the walk |
| `xfs_freesp_scrape_success` | gauge 0/1 | per-mount collection health |

### Phase 1.5

| Metric | Source | Note |
|---|---|---|
| `xfs_free_extent_size_bytes_bucket{le}` | GETFSMAP | cumulative count ≤ size (le = 4Ki, 64Ki, 1Mi, 32Mi, 1Gi, +Inf); shows distribution drift |
| `xfs_avg_free_extent_bytes` | GETFSMAP | EKS-NMA-equivalent; **HELP text documents the confound** |
| `xfs_size_bytes` / `xfs_avail_bytes` | statfs | capacity context |
| `xfs_inodes_used` / `xfs_inodes_total` | statfs | catch imaxpct exhaustion (a distinct real failure) |
| `xfs_agsize_bytes` / `xfs_agcount` / `xfs_block_size_bytes` | FSGEOMETRY | normalise MAX_EXT against agsize; may need `CAP_SYS_ADMIN` (verify) |
| `xfs_sparse_inodes_enabled` / `xfs_imaxpct` | FSGEOMETRY | config context; **constant across fleet — low priority** |

### Dropped (see §0)

`xfs_extent_alloc_*` / `xfs_extent_free_*` (node_exporter has them);
`baseline_ratio` rule; `xfs_node_uptime_seconds`.

## 4. Collection mechanism (native GETFSMAP)

- `FS_IOC_GETFSMAP = 0xc0c0583b`. Structs hand-defined (x/sys/unix has no
  helper): `fsmap_head` 192 B, `fsmap` 64 B; `fmr_physical` / `fmr_length` are in
  **bytes**. Reference implementation to copy the ~40 lines: `superfly/fsmap`
  (Apache-2.0) — copy, don't depend.
- Per mount: open `O_RDONLY`, set `fmh_count`, keys `[0]`=low (physical 0),
  `[1]`=high (all-ones owner/offset/flags, physical=end); loop the ioctl,
  advancing the low key past the last record each batch until `FMR_OF_LAST`.
- Keep records with `FMR_OF_SPECIAL_OWNER` set and `fmr_owner == FMR_OWN_FREE`.
  From `fmr_length` compute count, Σ bytes, max, and (1.5) buckets.
- `statfs` per mount for total/avail bytes and inode counts.
- **Validated on a live RHCOS node** (ROSA, OCP 4.20, kernel 5.14 el9, amd64):
  matched `xfs_spaceman -c 'freesp -s'` — ≈502 free extents, density ≈1.07,
  MAX_EXT ≈ agsize. Gotchas found only on-cluster:
  1. Query the whole device with the low key all-zero and the high key
     `device`/`physical`/`owner`/`offset` maxed — do **not** set the high key's
     `fmr_flags` (the kernel validates key flags).
  2. Free extents match by **`fmr_owner` low-32 == 1**: the kernel reports the
     bare `FMR_OWN_FREE` code (`0x1`), not the documented `FMR_OWNER('X',1)`
     (`0x5800000001`).
  3. Discover mounts from the container's own `/proc/self/mountinfo` (the host FS
     appears under `/host` via mount propagation) and dedupe by device — free
     space is per-filesystem, not per-mount.
  4. hostPath on OpenShift needs an SCC that allows it (`hostmount-anyuid`), and
     that SCC forbids setting `seccompProfile` (CRI-O applies RuntimeDefault
     anyway). `XFS_IOC_FSGEOMETRY` cap requirement still open (phase 1.5).

## 5. Rules & alerts (deferred — separate PrometheusRule file, not the binary)

Given `sparse=1`, alert only on states that genuinely gate failure. **Baked as
`deploy/prometheusrule.yaml`** (thresholds are heuristic defaults — calibrate):

| Alert | Expression | Severity | Meaning |
|---|---|---|---|
| `XFSFragmentationRising` | `node:xfs_frag_density:rate1d > 1.05` for 2h | warning | **sick (early):** fragmenting >3× the ~0.35/day aging baseline |
| `XFSLowContiguity` | `xfs_free_extent_max_bytes / agsize_bytes < 0.10` for 30m | warning | **approaching dead:** largest free extent collapsing vs AG ceiling |
| `XFSSparseInodesDisabled` | `xfs_sparse_inodes_enabled == 0` for 1h | warning | real danger — re-opens ENOSPC path (phase 1.5 metric) |
| `XFSExporterStale` | `xfs_freesp_scrape_success == 0` for 30m | info | collection failing |

Recording rule: `node:xfs_frag_density:rate1d = deriv(xfs_frag_density_extents_per_gib[24h]) * 86400`.
`1.05 = 3 × 0.35` is a *rate* multiplier (defensible even though the absolute 0.35
is 3 points); `0.10` and `agsize` (fleet constant `1,434,976,256 B` = 350336×4096
until the FSGEOMETRY metric ships) are heuristic. **None are fleet-calibrated.**
Peer-relative snapshot alternative (no history): `topk(10, xfs_frag_density_extents_per_gib)`.

## 6. Acceptance criteria

1. Static binary; `scratch` image builds and runs as a DaemonSet with **no
   `privileged` and no `CAP_SYS_ADMIN`** on a node with `rmapbt=0`.
2. `curl localhost:9101/metrics` returns valid exposition within 60 s of start;
   `promtool check metrics` passes.
3. **Parser fixture** (the one non-trivial unit): a synthetic raw-extent list of
   358 free extents, Σ = 74,739,205 blocks (306,131,783,680 B), max = 348,288
   blocks → `xfs_frag_density_extents_per_gib` = **1.2557 ± 0.001**,
   `xfs_free_extent_max_bytes` = **1,426,587,648**.
4. Auto-discovers **all** XFS mounts (e.g. both `/sysroot` and `/var`) and labels
   each by `mountpoint` + `device`.
5. Steady-state CPU < 10 m, RSS < 64 Mi; `xfs_freesp_scrape_duration_seconds`
   exported and observed < 5 s on the measured fs.
6. Creates no files on any host filesystem.
7. Tolerates all taints; runs on all Linux nodes (optionally gated to
   `node-role.kubernetes.io/worker`); survives reboot.

## 7. Deployment

**Stage 1 (generic, this cluster — no UWM):** Namespace, ServiceAccount,
DaemonSet, headless Service exposing `:9101/metrics`. Operator points Prometheus
at it (static config / pod annotations). ServiceMonitor + PrometheusRule shipped
as **separate optional files** for Prometheus-Operator users.

**Stage 2 (ROSA HCP specifics — all re-verified):**
- MachineConfig / Ignition / mkfs / mount changes are blocked by
  `regular-user-validation.managed.openshift.io` — tool stays a plain workload. ✓
- `NodePool.autoRepair` reacts only to `Ready=False` **and `Ready=Unknown`**, and
  ignores custom NodeConditions — so **no remediation via conditions**; metrics
  and alerts only. ✓
- UWM is available but must be enabled via the `cluster-monitoring-config`
  ConfigMap (`enableUserWorkload: true`); users need `monitoring-edit` +
  `monitoring-rules-edit`; PrometheusRule is evaluated by Thanos Ruler;
  Alertmanager is SRE-managed (use a user-managed Alertmanager for routing).
- If least-privilege fails on ROSA: `privileged` SCC needs **cluster-admin** to
  grant, and the namespace needs `security.openshift.io/scc.podSecurityLabelSync:
  "false"` + `pod-security.kubernetes.io/enforce: privileged`, and must not be
  `openshift-*`.

## 8. Out of scope / non-goals

- Predicting ENOSPC (prevented by `sparse=1`) — this tool observes, it does not
  predict a failure that no longer occurs.
- Remediation / node draining — autoRepair can't act on custom conditions; node
  replacement is a human decision.
- `xfs_fsr` — defragments *files*, not free space; can worsen free-space frag.
- Any change to mount options, mkfs geometry, or partition layout (needs
  Ignition, unreachable on ROSA HCP).
- `imaxpct` / per-AG inode-imbalance ENOSPC — a *different* real failure a free-
  extent histogram can't see; flagged for a future, separate detector.

## 9. Deliverables

1. Go source: `main`, GETFSMAP ioctl + struct, mount discovery, per-mount
   collector, in-memory snapshot + HTTP handler, parser unit test (§6.3).
2. `Dockerfile` (multi-stage → `scratch`).
3. `deploy/` manifests: Namespace, SA, DaemonSet, Service; optional
   `servicemonitor.yaml`, `prometheusrule.yaml`.
4. `README.md`: deploy steps, no-UWM scrape wiring, and
   `topk(10, xfs_frag_density_extents_per_gib)` to rank sick nodes.
