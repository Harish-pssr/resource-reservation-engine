// Package workload generates the simulated demand (D10): how many items a buyer wants, and which.
// It is pure and seeded, so a run can be repeated exactly and the shapes can be unit-tested.
package workload

import (
	"math"
	"math/rand/v2"
	"sort"
)

// SizeWeights are the agreed shares of requests wanting 1..6 items, in percent. They are an
// assumption for the simulation, not measured from real bookings.
var SizeWeights = [6]float64{20, 40, 15, 15, 5, 5}

// Picker draws item blocks. It is not safe for concurrent use: draw all picks up front.
type Picker struct {
	k   int       // number of items in the event
	cum []float64 // cum[i] = sum of rank^-s for ranks 1..i+1
	rng *rand.Rand
}

// NewPicker returns a picker over k items with Zipf exponent s (0 = uniform; larger = more skewed).
func NewPicker(k int, s float64, seed uint64) *Picker {
	cum := make([]float64, k)
	total := 0.0
	for r := 1; r <= k; r++ {
		total += math.Pow(float64(r), -s)
		cum[r-1] = total
	}
	return &Picker{k: k, cum: cum, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// Size draws how many items the next buyer wants.
func (p *Picker) Size() int {
	u := p.rng.Float64() * 100
	acc := 0.0
	for i, w := range SizeWeights {
		acc += w
		if u < acc {
			return i + 1
		}
	}
	return len(SizeWeights)
}

// Pick draws a size and then a block of that many contiguous items.
func (p *Picker) Pick() []int { return p.PickN(p.Size()) }

// PickN draws a block of n contiguous items: a Zipf draw chooses the anchor (the first item,
// 0-based), restricted so that the whole block fits inside the event.
func (p *Picker) PickN(n int) []int {
	if n > p.k {
		n = p.k
	}
	fits := p.k - n + 1 // anchors 0..fits-1 leave room for n items
	u := p.rng.Float64() * p.cum[fits-1]
	anchor := sort.SearchFloat64s(p.cum[:fits], u)
	if anchor >= fits {
		anchor = fits - 1
	}
	block := make([]int, n)
	for i := range block {
		block[i] = anchor + i
	}
	return block
}
