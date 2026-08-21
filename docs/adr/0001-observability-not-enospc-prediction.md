# 1. Reframe from ENOSPC prediction to fragmentation observability

Date: 2026-08-21
Status: Accepted

## Context

The v1 spec targeted the "`creat()` returns ENOSPC while `df` shows free space"
failure, caused by XFS free-space fragmentation starving 64-inode chunk
allocation (which classically needs 8 contiguous, aligned filesystem blocks).

Research and a real-node measurement refuted the premise for the target fleet:

- `sparse=1` is the mkfs.xfs default since xfsprogs 4.16 and is set on **every
  ROSA node** (confirmed via `xfs_info /var`: `sparse=1`, `rmapbt=0`, `imaxpct=25`).
- Sparse inodes drop the inode-chunk allocation floor from 8 contiguous blocks
  (32 KiB) to a **single 4 KiB block**, closing the fragmentation-driven
  inode-ENOSPC path.
- Red Hat KCS 7110315: the residual bug is RHEL 8 only — "no indication of this
  issue happening in RHEL 9." RHCOS (OCP 4.14–4.20) is RHEL 9.
- The realistic modern "free space but ENOSPC" causes (`imaxpct` ceiling, per-AG
  inode imbalance) are **not visible in a free-extent histogram** anyway.

So an exporter that predicts ENOSPC from free-space fragmentation would almost
never fire on a healthy node and would be blind to the real modern causes.

## Decision

Build the tool as **fragmentation observability**: expose per-node, per-mount
free-space fragmentation metrics to **rank and identify degrading ("sick")
nodes**, not to predict an ENOSPC failure that `sparse=1` prevents. Alerting is
deferred and limited to states that genuinely gate failure (MAX_EXT far below
agsize; `sparse=0` if ever seen; rising density rate).

## Consequences

- Primary output is metrics/trends, not alerts. `frag_density` is a ranking
  signal, not an alarm threshold.
- The v1 `baseline_ratio` rule and its hardcoded `0.35` (fit to 3 nodes, divided
  by reboot-resetting uptime) are dropped.
- Config metrics (`sparse`, `imaxpct`) are low value — constant across the fleet.
- `imaxpct`/inode-imbalance ENOSPC is acknowledged as a separate future detector.
- Reversible in principle, but reversing means re-adopting a failure model Red
  Hat and the platform's own defaults have engineered away — high bar.
