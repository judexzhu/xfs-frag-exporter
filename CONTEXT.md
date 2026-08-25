# Context — XFS Free-Space Fragmentation Exporter

Glossary of the load-bearing terms. Use these exact terms in issues, metrics,
tests, and code. No implementation detail here — see the SPEC and ADRs for that.

## Terms

- **Sick node** — a node whose XFS free space has fragmented enough to stand out
  from its peers: high `frag density` and/or `MAX_EXT` far below `agsize`. It is a
  *degrading/ranking* judgement, **not** an imminent-ENOSPC prediction (see
  [[ADR-0001]]). The tool's job is to identify these.

- **Free-space fragmentation** — the splitting of a filesystem's free space into
  many small, non-contiguous extents over time under container churn. Distinct
  from *file* fragmentation.

- **Free extent** — one contiguous run of free blocks, as reported by
  `GETFSMAP` (`fmr_owner == FMR_OWN_FREE`). Lengths are in bytes.

- **Small-extent fraction** (`xfs_free_extents_small / xfs_free_extents`) —
  the share of free extents smaller than 64 KiB (16 blocks). **The primary
  sick-node signal** (see [[ADR-0004]]): critical above 0.90, where XFS can
  return ENOSPC despite free space (RHEL-82924). Count-based, so it catches
  heavy-tail distributions the byte-average misses (field evidence: node-71
  had avg 424 KiB — above the floor — yet 97.6% of extents were tiny).

- **Average free extent size** (`xfs_free_extent_avg_bytes`) — `free_bytes /
  free_extents`, the mean contiguous free run. **Corroborating signal** (refined
  by [[ADR-0004]]): critical below 16 blocks (64 KiB), matching AWS EKS's
  `XFSSmallAverageClusterSize` (a provisional threshold — their TODO says
  "collect data to get an accurate value"). Can miss heavy-tail cases where a
  few large extents inflate the mean. The exact reciprocal of `frag density`
  (see [[ADR-0003]]).

- **frag density** (`frag_density`) — `free_extents / free_GiB`. For a fixed block
  size this is the exact reciprocal of average free extent size (`262144 /
  avg_blocks` at 4K) — same information, chosen for intuitiveness (higher = worse),
  not because it removes a confound. Its **level** is the reliable signal (critical
  above ~16384 extents/GiB, the reciprocal of the 64 KiB avg-extent floor); see
  [[ADR-0003]].

- **frag rate** — `deriv(frag_density[24h]) × 86400`, extents/GiB/day. A **weak,
  informational secondary** only: rate-based alerting is false-positive prone (a
  pending alert fired on a 6-hour-old node). Use the *level*, not the rate.

- **MAX_EXT** — the single largest free extent (`xfs_free_extent_max_bytes`).
  Gates large contiguous allocation. **Ceilinged at `agsize`** (a free extent
  cannot span an allocation group), so "healthy" ≈ agsize; only meaningful when
  it drops far below agsize.

- **Allocation group (AG) / agsize** — XFS divides a filesystem into AGs;
  `agsize` is the blocks per AG and the hard ceiling on any single free extent.

- **Sparse inodes** (`sparse=1`) — XFS feature (default on RHEL 9 / RHCOS) that
  lets inode chunks be allocated from as little as a single block, lowering the
  contiguous run inode allocation needs (a full 64-inode chunk → a smaller sparse
  cluster). It **mitigates but does not eliminate** fragmentation ENOSPC: under
  severe free-space fragmentation allocation still fails, and RHEL-82924 is
  confirmed on `sparse=1` ROSA HCP nodes. `sparse=0` aggravates it further.

- **GETFSMAP** — `XFS_IOC_GETFSMAP`, the only live kernel interface exposing the
  free-extent size distribution. Issued natively (see [[ADR-0002]]); needs no
  `CAP_SYS_ADMIN` for free space.
