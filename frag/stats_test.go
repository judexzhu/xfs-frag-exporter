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

func TestAggregateEmpty(t *testing.T) {
	s := Aggregate(nil)
	if s != (Stats{}) {
		t.Fatalf("Aggregate(nil) = %+v, want zero", s)
	}
}
