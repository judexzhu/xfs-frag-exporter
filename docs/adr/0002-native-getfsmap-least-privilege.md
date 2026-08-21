# 2. Native GETFSMAP ioctl, least-privilege, over chroot xfs_spaceman

Date: 2026-08-21
Status: Accepted

## Context

v1 proposed a privileged container that `chroot`s to the host and shells out to
`xfs_spaceman -c 'freesp -s'`, justified by (a) `GETFSMAP` needing `CAP_SYS_ADMIN`
and (b) the container's `xfsprogs` possibly mismatching the host kernel's ioctl
struct. Research found:

- `XFS_IOC_GETFSMAP` has **no capability gate** for free space. `CAP_SYS_ADMIN`
  only selects the reverse-map (`rmapbt`) backend that reveals inode owners;
  unprivileged callers use the free-space (`bnobt`) backend and read free extents
  fine. The measured nodes have `rmapbt=0`, so the privileged path isn't even
  available.
- The `fsmap`/`fsmap_head` ABI is frozen since Linux 4.12; a static binary with
  hand-defined structs is safe on any modern kernel. `x/sys/unix` has no helper;
  `superfly/fsmap` is a copyable ~40-line reference.
- `freesp` is merely GETFSMAP + userspace bucketing and never outputs the largest
  free extent — so shelling out gives *less* than the raw ioctl, at the cost of a
  fork + chroot + host-binary/library dependency + stdout parsing per scrape.

## Decision

Issue `XFS_IOC_GETFSMAP` **natively** from a static Go binary. Run the DaemonSet
**least-privilege**: hostPath `/` → `/host` read-only (`mountPropagation:
HostToContainer`), discover XFS mounts from `/host/proc/1/mountinfo`, open each
`O_RDONLY`, ioctl. **No `privileged`, no `CAP_SYS_ADMIN`, no chroot** on the MVP
path. If a cluster's SELinux/SCC blocks the hostPath open, fall back to
`privileged: true` and harden later.

## Consequences

- Exact largest-extent and arbitrary size buckets computed in-process from
  `fmr_length` (bytes) — no text parsing, no approximation.
- Depends only on the kernel syscall ABI; no host userspace surface per scrape.
- Adds ~40 lines of hand-maintained struct/ioctl code (no runtime dependency).
- Reading `sparse`/`imaxpct`/`agsize` needs `XFS_IOC_FSGEOMETRY` (phase 1.5),
  whose cap requirement must be verified; if it needs `CAP_SYS_ADMIN`, add only
  that one cap, not full `privileged`.
- Walk cost scales with fragmentation → 300 s interval regardless of mechanism.
