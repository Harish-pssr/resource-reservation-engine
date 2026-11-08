package bench

import (
	"errors"
	"slices"
	"testing"

	"resourcereservation/internal/loadgen"
)

func healthy() loadgen.Summary {
	return loadgen.Summary{Sent: 100, GeneratorOK: true, HoldP99Ms: 120}
}

func TestJudgeAppliesTheD12RulesInOrder(t *testing.T) {
	if v := Judge(healthy()); !v.Sustained {
		t.Fatalf("a healthy step must be sustained: %+v", v)
	}
	cases := []struct {
		name   string
		mutate func(*loadgen.Summary)
	}{
		{"generator behind", func(s *loadgen.Summary) { s.GeneratorOK = false; s.Skipped = 5 }},
		{"hold error", func(s *loadgen.Summary) { s.HoldErrors = 1 }},
		{"confirm error", func(s *loadgen.Summary) { s.ConfirmErrors = 2 }},
		{"room error", func(s *loadgen.Summary) { s.RoomErrors = 1 }},
		{"p99 at the bound", func(s *loadgen.Summary) { s.HoldP99Ms = 1000 }},
		{"p99 over the bound", func(s *loadgen.Summary) { s.HoldP99Ms = 4200 }},
		{"nothing sent", func(s *loadgen.Summary) { s.Sent = 0 }},
	}
	for _, c := range cases {
		s := healthy()
		c.mutate(&s)
		if v := Judge(s); v.Sustained || v.Reason == "" {
			t.Errorf("%s: must fail with a reason, got %+v", c.name, v)
		}
	}
}

// With the waiting room on, queue wait must not count against the store (D12).
func TestJudgeUsesStoreLatencyWhenTheRoomIsOn(t *testing.T) {
	s := healthy()
	s.RoomUsed, s.HoldP99Ms, s.StoreHoldP99Ms = true, 9000, 80 // buyers waited 9 s, the store answered in 80 ms
	if v := Judge(s); !v.Sustained {
		t.Fatalf("queue wait is reported separately and is not the store's latency: %+v", v)
	}
	s.StoreHoldP99Ms = 1500
	if v := Judge(s); v.Sustained {
		t.Fatalf("a slow store must still fail the step: %+v", v)
	}
}

func TestLadderIsGeometricStrictlyIncreasingAndBounded(t *testing.T) {
	got := Ladder(50, 1.25, 150)
	want := []float64{50, 63, 79, 99, 124}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Even a tiny factor must keep climbing instead of repeating a rate.
	if l := Ladder(1, 1.01, 5); !slices.Equal(l, []float64{1, 2, 3, 4, 5}) {
		t.Fatalf("got %v", l)
	}
}

func TestConfirmCapacityClimbsFromAPassingStart(t *testing.T) {
	var tested []int
	got := ConfirmCapacity(10, 3, func(i int) bool { tested = append(tested, i); return i <= 6 })
	if got != 6 {
		t.Fatalf("confirmed %d, want 6", got)
	}
	if !slices.Equal(tested, []int{3, 4, 5, 6, 7}) {
		t.Fatalf("each rate from the start up to the first failure is tested once: %v", tested)
	}
}

func TestConfirmCapacityWalksDownFromAFailingStart(t *testing.T) {
	var tested []int
	got := ConfirmCapacity(10, 8, func(i int) bool { tested = append(tested, i); return i <= 4 })
	if got != 4 || !slices.Equal(tested, []int{8, 7, 6, 5, 4}) {
		t.Fatalf("confirmed %d after testing %v", got, tested)
	}
}

func TestConfirmCapacityReportsNoneWhenNothingPasses(t *testing.T) {
	if got := ConfirmCapacity(4, 2, func(int) bool { return false }); got != -1 {
		t.Fatalf("got %d", got)
	}
	if got := ConfirmCapacity(0, 0, func(int) bool { return true }); got != -1 {
		t.Fatalf("an empty ladder: %d", got)
	}
	// A start beyond the ladder is clamped.
	if got := ConfirmCapacity(3, 99, func(i int) bool { return i == 2 }); got != 2 {
		t.Fatalf("clamped start: %d", got)
	}
}

func TestMajorityStopsAsSoonAsTheOutcomeIsDecided(t *testing.T) {
	run := func(seq ...bool) (func(int) (bool, error), *int) {
		calls := 0
		return func(rep int) (bool, error) { calls++; return seq[rep-1], nil }, &calls
	}
	cases := []struct {
		seq    []bool
		passed bool
		calls  int
	}{
		{[]bool{true, true, false}, true, 2},   // decided after two passes
		{[]bool{false, false, true}, false, 2}, // decided after two failures
		{[]bool{true, false, true}, true, 3},   // needs the third
		{[]bool{false, true, false}, false, 3},
	}
	for _, c := range cases {
		f, calls := run(c.seq...)
		passed, outcomes, err := Majority(f)
		if err != nil || passed != c.passed || *calls != c.calls || len(outcomes) != c.calls {
			t.Errorf("%v: passed %v after %d runs (outcomes %v), want %v after %d", c.seq, passed, *calls, outcomes, c.passed, c.calls)
		}
	}
	if _, _, err := Majority(func(int) (bool, error) { return false, errors.New("boom") }); err == nil {
		t.Fatal("an error must stop the vote and be returned")
	}
}
