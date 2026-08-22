package frag

import (
	"math"
	"testing"
)

// fixtureExtents reproduces the SPEC §6.3 acceptance fixture (node A):
// 358 free extents, 74,739,205 blocks total, largest 348,288 blocks (= 1,426,587,648
// bytes), 4 KiB bsize.
// GETFSMAP reports lengths in bytes, so everything is scaled by the block size.
func fixtureExtents() []uint64 {
	const (
		n     = 358
		total = uint64(74_739_205) * 4096 // 306,131,783,680 bytes
		max   = uint64(348_288) * 4096    // 1,426,587,648 bytes
	)
	ext := make([]uint64, n)
	ext[0] = max
	rest := total - max
	base := rest / (n - 1)
	for i := 1; i < n; i++ {
		ext[i] = base
	}
	ext[n-1] += rest - base*(n-1) // absorb the rounding remainder
	return ext
}

func TestAggregateFixture(t *testing.T) {
	s := Aggregate(fixtureExtents())
	if s.Extents != 358 {
		t.Errorf("Extents = %d, want 358", s.Extents)
	}
	if s.FreeBytes != 306_131_783_680 {
		t.Errorf("FreeBytes = %d, want 306131783680", s.FreeBytes)
	}
	if s.MaxBytes != 1_426_587_648 {
		t.Errorf("MaxBytes = %d, want 1426587648", s.MaxBytes)
	}
}

func TestDensityFixture(t *testing.T) {
	got := Aggregate(fixtureExtents()).Density()
	const want = 1.2557
	if math.Abs(got-want) > 0.001 {
		t.Fatalf("Density = %.4f, want %.4f ± 0.001", got, want)
	}
}

func TestDensityZeroFree(t *testing.T) {
	if d := (Stats{Extents: 5}).Density(); d != 0 {
		t.Fatalf("Density with zero free space = %v, want 0", d)
	}
}

func TestAvgExtentBytesFixture(t *testing.T) {
	got := Aggregate(fixtureExtents()).AvgExtentBytes()
	const want = 306_131_783_680.0 / 358 // ~855,116,713.6 bytes (~815 MiB, healthy)
	if math.Abs(got-want) > 1 {
		t.Fatalf("AvgExtentBytes = %.1f, want %.1f", got, want)
	}
}

// Density and AvgExtentBytes are exact reciprocals scaled by a GiB: their product
// is 2^30. This is the invariant that lets the EKS "avg < 16 blocks" threshold and
// the density threshold be one and the same signal.
func TestDensityAvgReciprocal(t *testing.T) {
	s := Aggregate(fixtureExtents())
	if got := s.Density() * s.AvgExtentBytes(); math.Abs(got-bytesPerGiB) > 1 {
		t.Fatalf("Density*AvgExtentBytes = %.1f, want 2^30 = %d", got, bytesPerGiB)
	}
}

func TestAvgExtentBytesZeroExtents(t *testing.T) {
	if a := (Stats{FreeBytes: 100}).AvgExtentBytes(); a != 0 {
		t.Fatalf("AvgExtentBytes with zero extents = %v, want 0", a)
	}
}

func TestAggregateEmpty(t *testing.T) {
	s := Aggregate(nil)
	if s != (Stats{}) {
		t.Fatalf("Aggregate(nil) = %+v, want zero", s)
	}
}
