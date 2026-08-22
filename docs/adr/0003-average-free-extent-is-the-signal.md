# 3. Average free-extent size (level) is the sick-node signal, not the frag rate

Date: 2026-08-21
Status: Accepted — refined by ADR-0004 (the count-fraction under 64 KiB, not the
byte-average, is the primary signal; the average is demoted to corroborating)

## Context

ADR-0001 reframed the tool as fragmentation *observability* on the premise that
`sparse=1` had engineered the "ENOSPC while `df` shows free space" failure away,
and §1(b) of the SPEC concluded that the **rate of change** of density — not its
level — was the sick-node discriminator (density scales with node age, so any
absolute threshold was thought to fire on old-but-normal nodes).

Field evidence overturns both premises:

- Red Hat kernel tracking **RHEL-82924** confirms fragmentation ENOSPC on
  `sparse=1` ROSA HCP nodes: `sparse=1` lowers the contiguous run inode allocation
  needs but does **not** eliminate the failure. `df -i` stays healthy — the root
  cause is free-space fragmentation, not inode exhaustion. The confirmed trigger is
  the Fluent Bit / fluentd filesystem buffer (kernel tracing: ~42× the XFS extent
  allocations of any other process); risk grows with node age.
- The rate-based approach is unreliable in practice: rate PromQL false-positives
  across production clusters (a pending alert fired on a 6-hour-old node).
- There is a validated absolute floor the aging trend never reaches on a healthy
  node — the same one AWS EKS's Node Monitoring Agent uses
  (`XFSSmallAverageClusterSize`: average free extent < 16 blocks).

Jira **RFE-9762** ("expose the detection metric via OpenShift Monitoring and enable
automated node remediation") tracks this; this exporter is its implementation.

## Decision

The primary, field-validated sick-node signal is the **level** of the **average
free-extent size**, exported as `xfs_free_extent_avg_bytes` (= free_bytes /
free_extents). It is **critical below 16 blocks (64 KiB)**. Because
`density = 2^30 / avg_extent_bytes` (exact reciprocal), the density level carries
the same signal (critical above ~16384 extents/GiB) — the two are one signal.
This matches EKS's `XFSSmallAverageClusterSize`.

The `deriv`-based frag-rate is **demoted to a weak, informational secondary** only.

## Consequences

- Alerts are repointed: `XFSSmallAverageExtent` (`xfs_free_extent_avg_bytes < 65536`
  for 30m) becomes the primary alert; `XFSFragmentationRising` (the density rate) is
  kept only as informational.
- `xfs_free_extent_avg_bytes` is added as an MVP metric (was previously slated for
  phase 1.5 under the name `xfs_avg_free_extent_bytes`).
- **Supersedes the "rate-not-level" reasoning of ADR-0001** and SPEC §1(b): the
  level is the reliable signal. ADR-0001's broader "observability, not ENOSPC
  prediction" framing still holds, but the tool now also flags a genuine ENOSPC
  risk that `sparse=1` mitigates rather than prevents.
- Remediation is node replacement (`xfs_fsr` needs an unmount): cordon → drain →
  delete the machine. Reduce the fragmentation rate preventively via Fluent Bit
  buffering limits and rotating long-lived nodes.
