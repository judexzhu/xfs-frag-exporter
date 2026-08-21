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

- **frag density** (`frag_density`) — `free_extents / free_GiB`. For a fixed block
  size this is the exact reciprocal of average free extent size (`262144 /
  avg_blocks` at 4K) — same information, chosen for intuitiveness (higher = worse),
  not because it removes a confound. It scales with node age, so the **rate of
  change** of density — not its level — is what identifies a sick node.

- **frag rate** — `deriv(frag_density[24h]) × 86400`, extents/GiB/day. The actual
  sick-node discriminator: a node fragmenting abnormally fast *for its age*.
  Aging baseline ≈ 0.35/day (3 nodes; heuristic, not calibrated).

- **MAX_EXT** — the single largest free extent (`xfs_free_extent_max_bytes`).
  Gates large contiguous allocation. **Ceilinged at `agsize`** (a free extent
  cannot span an allocation group), so "healthy" ≈ agsize; only meaningful when
  it drops far below agsize.

- **Allocation group (AG) / agsize** — XFS divides a filesystem into AGs;
  `agsize` is the blocks per AG and the hard ceiling on any single free extent.

- **Sparse inodes** (`sparse=1`) — XFS feature (default on RHEL 9 / RHCOS) that
  lets inode chunks be allocated from as little as a single block, removing the
  8-contiguous-block requirement. Its presence is why the classic ENOSPC failure
  no longer occurs on the target fleet (see [[ADR-0001]]).

- **GETFSMAP** — `XFS_IOC_GETFSMAP`, the only live kernel interface exposing the
  free-extent size distribution. Issued natively (see [[ADR-0002]]); needs no
  `CAP_SYS_ADMIN` for free space.
