#!/bin/bash
# ============================================================================
# Identify which process/pod is causing XFS free-space fragmentation.
#
# Two methods — pick the one that fits:
#
#   Method 1: ftrace (RECOMMENDED)
#     - Zero install, works on every RHCOS node
#     - Maps PID → pod namespace/name in one shot
#     - Run via: oc debug node/<node> -- chroot /host bash identify-culprit.sh
#
#   Method 2: bpftrace (richer output, needs dnf install)
#     - Needs CentOS Stream 9 image: oc debug node/<node> --image=quay.io/centos/centos:stream9
#     - Inside: dnf install -y bpftrace && mount -t debugfs debugfs /sys/kernel/debug
#     - PID → pod mapping requires a SEPARATE oc debug session (no crictl in container)
#     - See "BPFTRACE COMMANDS" section below
#
# WHAT IT TRACES:
#   XFS extent allocation tracepoints in the kernel. Every time XFS allocates
#   a block (for file data, inode chunks, metadata), the kernel fires a
#   tracepoint. The process with the most allocation events is the one
#   creating the most fragmentation pressure.
#
#   Tracepoints used (RHEL 9 / kernel 5.14+):
#
#   xfs_alloc_near_first
#     Fires during the AG search for free space. May fire multiple times per
#     allocation request as XFS searches through AGs. Highest event count —
#     good for identifying the dominant allocator. This is what this script
#     uses by default.
#
#   xfs_alloc_vextent_near_bno
#     Entry point — one event per allocation request. Lower count but more
#     precise. Fields include minlen/alignment which distinguish data
#     allocation (alignment=1) from inode chunk allocation (alignment=8,
#     minlen=8 on sparse=0; alignment=1, minlen=1 on sparse=1 when
#     fragmented).
#
#   xfs_alloc_vextent_allfailed
#     Fires when allocation fails across ALL AGs — the actual ENOSPC moment.
#     Trace this to see which process triggers the failure.
#
#   xfs_alloc_file_space
#     Only file data allocation (fallocate). Not inode allocation.
#
#   Note: xfs_alloc_extent (recommended in newer docs) does NOT exist on
#   RHEL 9 kernel 5.14. Use the tracepoints above instead.
#
# On every RHEL-82924 case so far, the top allocator was fluent-bit / fluentd.
# ============================================================================

set -eu

DURATION=${1:-10}
TRACEPOINT=${2:-xfs_alloc_near_first}

echo "== XFS fragmentation culprit finder (ftrace) =="
echo "   Tracing $TRACEPOINT for ${DURATION}s..."
echo ""

# Verify tracepoint exists
if [ ! -d "/sys/kernel/debug/tracing/events/xfs/$TRACEPOINT" ]; then
  echo "ERROR: tracepoint xfs:$TRACEPOINT not found"
  echo "Available: $(ls /sys/kernel/debug/tracing/events/xfs/ | grep alloc | tr '\n' ' ')"
  exit 1
fi

# Clear stale trace buffer
echo > /sys/kernel/debug/tracing/trace
echo 1 > /sys/kernel/debug/tracing/events/xfs/$TRACEPOINT/enable

timeout "$DURATION" cat /sys/kernel/debug/tracing/trace_pipe > /tmp/xfs_trace.txt 2>/dev/null

echo 0 > /sys/kernel/debug/tracing/events/xfs/$TRACEPOINT/enable

TOTAL=$(wc -l < /tmp/xfs_trace.txt)
echo "   Captured $TOTAL allocation events in ${DURATION}s"
echo ""

if [ "$TOTAL" -eq 0 ]; then
  echo "   No XFS allocations detected. The fragmenter may be idle."
  echo "   Try a longer duration: bash $0 60"
  echo "   Or trace during active I/O."
  rm -f /tmp/xfs_trace.txt
  exit 0
fi

echo "== Top XFS allocators =="
printf "%8s  %-15s  %-10s  %s\n" "ALLOCS" "PROCESS" "PID" "POD"
printf "%8s  %-15s  %-10s  %s\n" "------" "-------" "---" "---"

awk '{split($1,a,"-"); pid=a[length(a)]; comm=$1; gsub(/-[0-9]+$/,"",comm); print pid, comm}' /tmp/xfs_trace.txt | \
  sort | uniq -c | sort -rn | head -10 | while read count pid comm; do
    cid=$(sed -n 's|.*crio-\([a-f0-9]*\)\.scope|\1|p' /proc/$pid/cgroup 2>/dev/null)
    if [ -n "$cid" ]; then
      pod=$(crictl inspect --output go-template \
        --template '{{index .status.labels "io.kubernetes.pod.namespace"}}/{{index .status.labels "io.kubernetes.pod.name"}}' \
        "${cid:0:13}" 2>/dev/null)
      printf "%8d  %-15s  %-10s  %s\n" "$count" "$comm" "$pid" "$pod"
    else
      printf "%8d  %-15s  %-10s  %s\n" "$count" "$comm" "$pid" "(host)"
    fi
  done

echo ""
echo "== The top allocator is your likely fragmenter =="

rm -f /tmp/xfs_trace.txt

# ============================================================================
# BPFTRACE COMMANDS (Method 2)
#
# Prerequisites (run once per debug session):
#   oc debug node/<node> --image=quay.io/centos/centos:stream9
#   dnf install -y bpftrace
#   mount -t debugfs debugfs /sys/kernel/debug
#
# Basic — count allocations per process (30s):
#   timeout 30 bpftrace -e '
#     tracepoint:xfs:xfs_alloc_near_first { @[comm, pid] = count(); }'
#
# Allocation requests per process (one event per request, not per AG search):
#   timeout 30 bpftrace -e '
#     tracepoint:xfs:xfs_alloc_vextent_near_bno { @[comm, pid] = count(); }'
#
# Show allocation failures (the ENOSPC moment):
#   timeout 30 bpftrace -e '
#     tracepoint:xfs:xfs_alloc_vextent_allfailed { @[comm, pid] = count(); }'
#
# Rich — allocations + distinguish inode vs data by alignment:
#   timeout 30 bpftrace -e '
#     tracepoint:xfs:xfs_alloc_vextent_near_bno {
#       @allocs[comm, pid] = count();
#       @by_alignment[comm, args->alignment, args->minlen] = count();
#     }'
#
# Map PID → pod (run from a SEPARATE oc debug session with chroot /host):
#   PID=<pid from bpftrace output>
#   CID=$(sed -n 's|.*crio-\([a-f0-9]*\)\.scope|\1|p' /proc/$PID/cgroup)
#   crictl inspect --output go-template \
#     --template '{{index .status.labels "io.kubernetes.pod.namespace"}}/{{index .status.labels "io.kubernetes.pod.name"}}' \
#     "${CID:0:13}"
# ============================================================================
