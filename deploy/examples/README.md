# Reproducers — validating that the exporter catches the real bug

Two manifests here reproduce the XFS free-space-fragmentation problem that hits
long-lived OpenShift / ROSA HCP nodes (Red Hat kernel tracking **RHEL-82924**,
Jira **RFE-9762**), and prove `xfs-frag-exporter` detects it.

| File | Goal | Verified? |
|---|---|---|
| [`reproducer.yaml`](./reproducer.yaml) | Prove **detection** — drive avg free extent below the danger floor and show the exporter reports it, matching `xfs_spaceman` | **Yes** (live) |
| [`reproducer-enospc.yaml`](./reproducer-enospc.yaml) | The actual **`creat()` ENOSPC with free space** on `sparse=1`, mimicking Fluent Bit small-file churn | **Yes** (live) |

## The concept

**Symptom:** XFS returns `ENOSPC` ("no space left on device") while `df`/`du`
still show many free GiB and `df -i` shows free inodes. On ROSA HCP this is
confirmed to be **free-space fragmentation**, not inode exhaustion, and the
**trigger is the Fluent Bit / fluentd filesystem buffer** — its create/delete
churn of many small files scatters free space into tiny, non-contiguous extents
over weeks. (Kernel tracing on affected nodes showed fluent-bit generating ~42×
more XFS extent allocations than any other process.)

**Why `sparse=1` (the ROSA HCP default, which customers cannot change) is not
immunity.** XFS allocates inodes in 64-inode chunks. Without sparse inodes a
chunk needs **8 contiguous aligned blocks** (32 KiB). Sparse inodes lower that to
**one inode cluster = 2 contiguous blocks** (8 KiB, at 512 B inodes) — but not to
zero. So once free space is fragmented to **isolated single 4 KiB blocks with no
2-block run left anywhere**, even a sparse inode cluster can't be allocated and
`creat()` fails with `ENOSPC` while free single blocks remain.

```
healthy:   [free 1 GiB contiguous..................]   creat() OK
fragmented:[F][X][F][X][F][X][F][X][F][X][F][X][F][X]   no 2-block run -> creat() ENOSPC
           F = free 4 KiB block   X = allocated 4 KiB block
```

## How the exporter finds it

`xfs-frag-exporter` issues `XFS_IOC_GETFSMAP` and aggregates every free extent, so
it can measure the mean contiguous free-extent size directly:

```
xfs_free_extent_avg_bytes = xfs_free_bytes / xfs_free_extents
```

When that average falls below **16 blocks (64 KiB)**, XFS is in the danger zone.
This is the same quantity **AWS EKS's Node Monitoring Agent (NMA)** alerts on,
named `XFSSmallAverageClusterSize` (threshold: avg free extent < 16 blocks) —
OpenShift has no built-in equivalent, which is what RFE-9762 is asking for.

The reproducers cross-check the exporter against `xfs_spaceman -c 'freesp -s'`
ground truth. In the verified `reproducer.yaml` run (on a `sparse=1` fs):

| Source | Average free extent |
|---|---|
| `xfs_spaceman freesp -s` | `1.14908` blocks |
| exporter `xfs_free_extent_avg_bytes` | `4706 B` (= 1.149 × 4096) — **exact match** |

with `xfs_frag_density_extents_per_gib = 228134` (≫ the ~16384 critical level).

### The alert (NMA parity)

Shipped in [`deploy/prometheusrule.yaml`](../prometheusrule.yaml) /
`--set prometheusRule.enabled=true`:

```yaml
- alert: XFSSmallAverageExtent          # == AWS NMA XFSSmallAverageClusterSize
  expr: xfs_free_extent_avg_bytes < 65536   # 16 blocks x 4 KiB
  for: 30m
```
PromQL for ad-hoc checks:
```promql
xfs_free_extent_avg_bytes < 65536             # at/under the 16-block floor
topk(10, xfs_frag_density_extents_per_gib)    # rank worst mounts (reciprocal signal)
```

## How to use

Both need the `privileged` SCC (loop device + `mount`) — run on a **throwaway /
test node**. Each writes a small loopback image to node `/var` (emptyDir),
auto-removed on delete.

```sh
# Prove detection (fast, safe, verified):
oc apply -f deploy/examples/reproducer.yaml
oc -n xfs-repro logs -f xfs-repro          # watch freesp -s vs exporter metrics

# Attempt the actual ENOSPC (mimics fluentd churn):
oc apply -f deploy/examples/reproducer-enospc.yaml
oc -n xfs-repro logs -f xfs-enospc

# Clean up (detach the host loop device, then remove the namespace):
oc -n xfs-repro exec <pod> -- sh -c 'umount /mnt/x; losetup -D'
oc delete ns xfs-repro
```

Knobs (pod `env`): `FS_GB` sizes the loopback filesystem.

### Verified result (sparse=1 loopback)

`reproducer-enospc.yaml`, run to completion:

```
creat() failed: /mnt/x/churn/f_73149: No space left on device
df -h:  /dev/loop2  960M  554M  407M  58%     <- 42% of blocks FREE
df -i:  /dev/loop2  524288 78784 445504 16%   <- 84% of inodes FREE
freesp -s: 113,886 free extents, average 1.00003 blocks
exporter:  xfs_free_extent_avg_bytes = 4096   (density 262137)  <- matches freesp -s
```

ENOSPC with 42% of blocks and 84% of inodes free — pure free-space fragmentation
on `sparse=1`. Exactly RHEL-82924. The exporter's `xfs_free_extent_avg_bytes`
tracked the ground truth to the block and sat far below the 64 KiB alert floor.

> Note: why it must PUNCH, not delete — `rm` frees the inode slot too, so plain
> create/delete churn recycles inodes and slides into ordinary "disk full"
> instead. Freeing data while keeping the inode (truncate/punch, as log rotation
> does) is what strands the inodes and forces the failing new-cluster allocation.
