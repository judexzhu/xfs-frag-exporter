# 4. The fraction of free extents under 64 KiB is the primary signal, not the average

Date: 2026-08-22
Status: Accepted (refines ADR-0003)

## Context

ADR-0003 made the **average free-extent size** (`xfs_free_extent_avg_bytes`, with
`density` its exact reciprocal) the primary sick-node signal, critical below 16
blocks (64 KiB), matching AWS EKS's `XFSSmallAverageClusterSize`.

Customer field evidence from live ENOSPC incidents (four nodes) shows the average
is not sufficient on its own. Measured with `xfs_db -c frag` and
`xfs_spaceman -c "freesp -s"`:

| Node | Fragmentation factor | Free extents ≤ 60 KiB |
|---|---|---|
| `ip-10-26-86-145` | 33.18% | 96.3% |
| `ip-10-26-86-71` | 74.17% | 97.6% |
| `ip-10-26-86-34` | 93.98% | 95.3% |

`df -i` was ~1% on every node — free-space fragmentation, not inode exhaustion.

The decisive case is `ip-10-26-86-71`: **264 GB free across 652,406 free extents,
97.6% of them under 60 KiB — yet the average free extent was ~424 KiB, ABOVE the
64 KiB floor.** A handful of large extents held most of the free *bytes*, so the
byte-mean stayed healthy while almost every extent was too small to allocate an
inode cluster. `XFSSmallAverageExtent` would not have fired on the customer's own
live-incident node. The signal the customer actually used was the **fraction of
free extents below ~60 KiB** — a count, not a mean.

## Decision

The primary, field-validated sick-node signal is the **fraction of free extents
smaller than 16 blocks (64 KiB)**: `xfs_free_extents_small / xfs_free_extents`.
A new gauge `xfs_free_extents_small` counts free extents below 64 KiB (extents are
4 KiB multiples, so this is the "≤ 60 KiB" bucket the customer reported). The
primary alert `XFSFreeSpaceFragmented` fires above **0.90** (incident nodes:
0.95–0.98).

`xfs_free_extent_avg_bytes` (and its reciprocal `density`) is **retained as a
corroborating LEVEL signal** — `XFSSmallAverageExtent` still ships and still
matches EKS — but it is no longer the sole primary, because it can miss a heavy
tail of large free extents.

## Consequences

- `xfs_free_extents_small` is added (7 → but really the count pairs with the
  existing `xfs_free_extents` so the ratio is derived in PromQL — no ratio metric).
- Alert precedence: `XFSFreeSpaceFragmented` (fraction > 0.90) is primary;
  `XFSSmallAverageExtent` (avg < 64 KiB) is corroborating; the density rate stays
  informational (unchanged from ADR-0003).
- **Refines ADR-0003:** the *level, not rate* conclusion still holds, but the
  robust level is the count-fraction, not the byte-average. ADR-0003's average
  threshold remains valid as a corroborating check.
- `reproducer-node-var.yaml` recreates the `ip-10-26-86-71` shape on a node's real
  `/var` (bounded fragmentation: millions of 4 KiB free shards while GiB stay free)
  so the fraction fires while the average does not — validating the new signal end
  to end against the deployed exporter.
