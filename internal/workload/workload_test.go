package workload

import (
	"math"
	"slices"
	"testing"
)

func TestSizesFollowTheAgreedShares(t *testing.T) {
	p := NewPicker(1000, 1.1, 1)
	const n = 200000
	var count [7]int
	for i := 0; i < n; i++ {
		count[p.Size()]++
	}
	for size := 1; size <= 6; size++ {
		got := float64(count[size]) / n * 100
		if math.Abs(got-SizeWeights[size-1]) > 0.7 {
			t.Errorf("size %d: %.2f%%, want about %.0f%%", size, got, SizeWeights[size-1])
		}
	}
}

func TestBlocksAreContiguousDistinctAndFit(t *testing.T) {
	const k = 50
	p := NewPicker(k, 1.4, 2)
	for i := 0; i < 20000; i++ {
		b := p.Pick()
		if len(b) < 1 || len(b) > 6 {
			t.Fatalf("block size %d", len(b))
		}
		if b[0] < 0 || b[len(b)-1] >= k {
			t.Fatalf("block %v leaves the event [0,%d)", b, k)
		}
		for j := 1; j < len(b); j++ {
			if b[j] != b[j-1]+1 {
				t.Fatalf("block %v is not contiguous", b)
			}
		}
	}
}

func TestAnchorShareMatchesTheZipfModel(t *testing.T) {
	// With n = 1 the anchor is a plain Zipf draw, so the hottest item's share must match the
	// model: 1/H(K, s). (The model value for K=1000, s=1.1 is about 17.9%.)
	const k, s, draws = 1000, 1.1, 300000
	h := 0.0
	for r := 1; r <= k; r++ {
		h += math.Pow(float64(r), -s)
	}
	want := 1 / h * 100
	p := NewPicker(k, s, 3)
	hits := 0
	for i := 0; i < draws; i++ {
		if p.PickN(1)[0] == 0 {
			hits++
		}
	}
	got := float64(hits) / draws * 100
	if math.Abs(got-want) > 0.5 {
		t.Fatalf("hottest item share %.2f%%, model says %.2f%%", got, want)
	}
}

func TestZeroSkewIsUniform(t *testing.T) {
	const k, draws = 100, 200000
	p := NewPicker(k, 0, 4)
	var count [k]int
	for i := 0; i < draws; i++ {
		count[p.PickN(1)[0]]++
	}
	want := float64(draws) / k
	for item, c := range count {
		if math.Abs(float64(c)-want) > want*0.15 {
			t.Fatalf("item %d drawn %d times, want about %.0f", item, c, want)
		}
	}
}

func TestSameSeedSameSequence(t *testing.T) {
	a, b := NewPicker(500, 1.1, 42), NewPicker(500, 1.1, 42)
	for i := 0; i < 1000; i++ {
		if !slices.Equal(a.Pick(), b.Pick()) {
			t.Fatalf("sequences diverged at draw %d", i)
		}
	}
	c := NewPicker(500, 1.1, 43)
	same := 0
	for i := 0; i < 100; i++ {
		if slices.Equal(NewPicker(500, 1.1, 42).Pick(), c.Pick()) {
			same++
		}
	}
	if same == 100 {
		t.Fatal("different seeds must give different sequences")
	}
}

func TestBlockLargerThanEventIsClamped(t *testing.T) {
	p := NewPicker(3, 1.1, 5)
	if b := p.PickN(6); len(b) != 3 || b[0] != 0 {
		t.Fatalf("got %v", b)
	}
}
