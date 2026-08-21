// Package frag holds the pure free-space aggregation logic — no syscalls, so it
// builds and tests on any OS. The GETFSMAP collection that feeds it is Linux-only
// and lives in the root package.
package frag

const bytesPerGiB = 1 << 30

// Stats summarises a filesystem's free extents.
type Stats struct {
	Extents   uint64 // number of free extents
	FreeBytes uint64 // total free space (sum of extent lengths)
	MaxBytes  uint64 // largest single free extent
}

// Aggregate reduces free-extent byte lengths to summary stats.
func Aggregate(extentBytes []uint64) Stats {
	var s Stats
	for _, n := range extentBytes {
		s.Extents++
		s.FreeBytes += n
		if n > s.MaxBytes {
			s.MaxBytes = n
		}
	}
	return s
}

// Density is free extents per GiB of free space (extents/GiB). It is the exact
// reciprocal of average free-extent size; its *rate of change* — not its level —
// is what identifies a fragmenting ("sick") node. Returns 0 when there is no
// free space, to avoid a divide-by-zero.
func (s Stats) Density() float64 {
	if s.FreeBytes == 0 {
		return 0
	}
	return float64(s.Extents) / (float64(s.FreeBytes) / bytesPerGiB)
}
