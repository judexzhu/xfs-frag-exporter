# Reproducer & Diagnostic Tools

## Overview

This directory contains a reproducer for the XFS free-space fragmentation
ENOSPC bug (RHEL-82924) and a diagnostic script to identify the culprit
process on affected nodes.

| File | Purpose |
|---|---|
| [`reproducer-node-var.yaml`](./reproducer-node-var.yaml) | Reproduce RHEL-82924 on real /var — gradual fragmentation → alert → ENOSPC |
| [`identify-culprit.sh`](./identify-culprit.sh) | Identify which process/pod is causing fragmentation (ftrace + bpftrace) |

## The bug (RHEL-82924)

XFS returns `ENOSPC` while `df` shows free GiB and `df -i` shows free inodes.
This is **free-space fragmentation**, not disk-full or inode exhaustion.

**Customer field evidence:**

| Node | Free space | Free extents | Extents < 60 KiB | Avg extent |
|---|---|---|---|---|
| ip-10-26-86-71 | 264 GB | 652,406 | **97.6%** | 424 KiB (above 64 KiB floor!) |
| ip-10-26-86-145 | — | — | 96.3% | — |
| ip-10-26-86-34 | — | — | 95.3% | — |

The byte-average (424 KiB) **missed** the problem — a few large free extents
inflated the mean. The count-based signal (97.6% tiny) caught it.

**Root cause:** Fluent Bit's `CIO_TRIM_FILES` behavior — truncate chunk files
to zero, then rewrite — keeps inodes alive while freeing data blocks. New
writes scatter across AGs. Over weeks, free space fragments into millions of
isolated single-block extents. When inode chunk allocation needs 2+ contiguous
blocks and can't find them in any AG → ENOSPC with free space remaining.

## Reproducer

### What scenario it reproduces

The reproducer recreates the exact field-evidence state: 99%+ of free extents
are smaller than 64 KiB while GiB remain free. It then drives `creat()` until
actual ENOSPC — the full RHEL-82924 failure chain.

It is designed as a **monitoring demo** showing the exporter catching the
problem before ENOSPC hits:

```
Phase 1: Fill     → fallocate REGION_GB on /var          (seconds)
Phase 2: Punch    → 10 gradual rounds, 120s pause each   (~20 minutes)
  round 1  → ratio ~10%  (healthy)
  round 5  → ratio ~50%  (degrading)
  round 9  → ratio ~90%  → XFSFreeSpaceFragmented FIRES
  round 10 → ratio ~99%  (critical)
Phase 3: creat()  → empty files until ENOSPC             (minutes-hours)
```

### Why this approach

**Decision: fill + punch, not truncate + rewrite churn**

We initially tried the Fluent Bit churn pattern (truncate + rewrite 350K files)
on real /var. It didn't work for a fast reproducer because:

- On a 300 GB disk at <20% usage, XFS has abundant contiguous space. Delayed
  allocation (`delalloc`) coalesces small writes into large extents. The
  small-extent ratio actually **dropped** during churn.
- The production bug takes **weeks** of churn to manifest. A reproducer that
  takes weeks isn't useful for demos or validation.

Instead we adopted the canonical **xfstests/xfs/076** pattern (Brian Foster,
Red Hat, 2015) — the kernel's own test for this exact failure mode:

1. Fill the region to 100% with one file (`fallocate`)
2. Punch alternating holes (`FALLOC_FL_PUNCH_HOLE`) so every free extent is
   isolated and smaller than an inode chunk
3. Allocate inodes until ENOSPC

This creates the **same free-space distribution** as weeks of churn, in
minutes. The gradual 10-round punching was added so the exporter scrapes the
climbing ratio and the `XFSFreeSpaceFragmented` alert fires before ENOSPC —
demonstrating the monitoring value.

### References

| Source | What we used | Link |
|---|---|---|
| xfstests/xfs/076 | Fill + punch + creat pattern | [kdave/xfstests tests/xfs/076](https://github.com/kdave/xfstests/blob/acb6d4cb/tests/xfs/076) |
| fluent-bit chunkio test | Confirmed truncate-on-close causes fragmentation | [fluent/fluent-bit lib/chunkio/tests/fs_fragmentation.c](https://github.com/fluent/fluent-bit/blob/51af5494/lib/chunkio/tests/fs_fragmentation.c) |
| EKS Node Monitoring Agent | Threshold reference (avg < 16 blocks, provisional) | [aws/eks-node-monitoring-agent monitors/storage/monitor.go:225](https://github.com/aws/eks-node-monitoring-agent/blob/51af5494/monitors/storage/monitor.go#L225) |
| amazon-eks-ami #1224 | Field evidence of ENOSPC from free-space fragmentation | [awslabs/amazon-eks-ami#1224](https://github.com/awslabs/amazon-eks-ami/issues/1224) |
| fluent-bit #7034 | Fluent Bit issue tracking the XFS fragmentation problem | [fluent/fluent-bit#7034](https://github.com/fluent/fluent-bit/issues/7034) |
| Red Hat KCS 7110315 | ENOSPC when free space is highly fragmented | [access.redhat.com/solutions/7110315](https://access.redhat.com/solutions/7110315) |

### Usage

Edit `nodeName` in the YAML. Deploy the exporter with enough memory for
millions of extents:

```sh
helm upgrade xfs-frag-exporter ./chart/xfs-frag-exporter -n default \
  --set rbac.sccBinding=true --set clusterMonitoring.enabled=true \
  --set prometheusRule.enabled=true --set resources.limits.memory=512Mi
```

Deploy and watch:

```sh
oc apply -f deploy/examples/reproducer-node-var.yaml
oc logs -f xfs-var-fragmenter

# Monitor in Prometheus:
#   xfs_free_extents_small{node="<node>"} / xfs_free_extents{node="<node>"}
```

### Configuration

| Env var | Default | Meaning |
|---|---|---|
| `REGION_GB` | 200 | Size of region to fragment. After punching, ~REGION_GB/2 is free-but-fragmented |
| `ROUNDS` | 10 | Number of punch rounds (controls gradient for demo) |
| `ROUND_PAUSE_SEC` | 120 | Seconds between rounds for exporter to scrape |

### Success criteria

1. `XFSFreeSpaceFragmented` fires (ratio > 0.90) **before** ENOSPC
2. Actual ENOSPC on `creat()`
3. `df` still shows free GiB at the moment of ENOSPC
4. Exporter metrics match field evidence shape (99%+ small extents)

## Diagnostic: identify the culprit

When `XFSFreeSpaceFragmented` fires on a customer node, the next question is
**"which process/pod is causing it?"**

`identify-culprit.sh` answers this using kernel ftrace — zero install, works
on any RHCOS node. It traces XFS extent allocations for a configurable duration
and maps each PID to its pod namespace/name via cgroup → CRI-O.

### How it works

The script enables the `xfs_alloc_near_first` kernel tracepoint via
`/sys/kernel/debug/tracing/`. Every time XFS allocates a block (data, inode
chunk, or metadata), the kernel records which process requested it. After
the trace window, the script counts events per PID and resolves each to a pod.

### Method 1: ftrace (recommended — zero install)

```sh
# Copy script to node and run:
oc debug node/<sick-node> -- chroot /host bash identify-culprit.sh

# Or inline:
oc debug node/<sick-node> -- chroot /host bash -c '
  echo > /sys/kernel/debug/tracing/trace
  echo 1 > /sys/kernel/debug/tracing/events/xfs/xfs_alloc_near_first/enable
  timeout 10 cat /sys/kernel/debug/tracing/trace_pipe > /tmp/t.txt 2>/dev/null
  echo 0 > /sys/kernel/debug/tracing/events/xfs/xfs_alloc_near_first/enable
  awk "{split(\$1,a,\"-\"); pid=a[length(a)]; comm=\$1; gsub(/-[0-9]+$/,\"\",comm); print pid, comm}" /tmp/t.txt | sort | uniq -c | sort -rn | head -5
'
```

Example output:

```
  1539  frag             3807125     default/xfs-var-fragmenter
     2  kworker/0:0      12345       (host)
     1  kworker/u16:2    67890       (host)
```

### Method 2: bpftrace (richer detail, needs dnf install)

bpftrace is **not installed** on RHCOS and the node has **no Red Hat
subscription** (ROSA HCP). Use a CentOS Stream 9 debug image instead:

```sh
oc debug node/<sick-node> --image=quay.io/centos/centos:stream9
# Inside:
dnf install -y bpftrace
mount -t debugfs debugfs /sys/kernel/debug

# Count allocations per process (30s):
timeout 30 bpftrace -e '
  tracepoint:xfs:xfs_alloc_near_first { @[comm, pid] = count(); }'

# Distinguish inode vs data allocation:
timeout 30 bpftrace -e '
  tracepoint:xfs:xfs_alloc_vextent_near_bno {
    @allocs[comm, pid] = count();
    if (args->alignment == 8 && args->minlen == 8) {
      @inode_allocs[comm, pid] = count();
    }
  }'

# Catch allocation failures (the ENOSPC moment):
timeout 30 bpftrace -e '
  tracepoint:xfs:xfs_alloc_vextent_allfailed { @[comm, pid] = count(); }'
```

**Note:** bpftrace runs inside the CentOS container but `crictl` is on the
host. Map PID → pod from a **separate** `oc debug node` session:

```sh
oc debug node/<sick-node>
chroot /host
PID=<pid from bpftrace output>
CID=$(sed -n 's|.*crio-\([a-f0-9]*\)\.scope|\1|p' /proc/$PID/cgroup)
crictl inspect --output go-template \
  --template '{{index .status.labels "io.kubernetes.pod.namespace"}}/{{index .status.labels "io.kubernetes.pod.name"}}' \
  "${CID:0:13}"
```

### Tracepoints reference

| Tracepoint | Fires when | Use for |
|---|---|---|
| `xfs_alloc_near_first` | XFS searches an AG for free space | General allocator pressure (default) |
| `xfs_alloc_vextent_near_bno` | One per allocation request (entry point) | Precise count; has minlen/alignment fields |
| `xfs_alloc_vextent_allfailed` | All AGs exhausted — allocation fails | Catching the ENOSPC-causing process |
| `xfs_alloc_file_space` | File data allocation (fallocate) | File-only, excludes inode allocation |

Note: `xfs_alloc_extent` (referenced in some docs) does **not exist** on RHEL
9 kernel 5.14. Use the tracepoints above.

### References

| Source | Link |
|---|---|
| KCS: How to run bpftrace on OpenShift 4 | [access.redhat.com/solutions/7105671](https://access.redhat.com/solutions/7105671) |
| KCS: What is bpftrace | [access.redhat.com/solutions/6904611](https://access.redhat.com/solutions/6904611) |
| KCS: Running BPFTrace on OCP4 for network analysis | [access.redhat.com/articles/7146080](https://access.redhat.com/articles/7146080) |

## Legacy reproducers (retired)

`reproducer.yaml` and `reproducer-enospc.yaml` used synthetic punch-hole
fragmentation on isolated loopback filesystems. They proved detection works
but don't match the real-world scenario or demonstrate the monitoring story.
Kept for reference only.

## Alerts

```yaml
- alert: XFSFreeSpaceFragmented          # PRIMARY — count-based
  expr: xfs_free_extents_small / xfs_free_extents > 0.90
  for: 30m
- alert: XFSSmallAverageExtent           # corroborating
  expr: xfs_free_extent_avg_bytes < 65536
  for: 30m
```
