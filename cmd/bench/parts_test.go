package main

import (
	"strings"
	"testing"

	"resourcereservation/internal/bench"
)

func TestPoolSizeDefaultsTo32AndFollowsTheProfile(t *testing.T) {
	t.Setenv("BENCH_DB_POOL_SIZE", "")
	if got := poolSize(); got != 32 {
		t.Fatalf("default pool size %d, want 32", got)
	}
	t.Setenv("BENCH_DB_POOL_SIZE", "8")
	if got := poolSize(); got != 8 {
		t.Fatalf("pool size %d, want 8", got)
	}
	for _, bad := range []string{"0", "-3", "many"} {
		t.Setenv("BENCH_DB_POOL_SIZE", bad)
		if got := poolSize(); got != 32 {
			t.Fatalf("%q must fall back to 32, got %d", bad, got)
		}
	}
}

func TestPartTextShowsPercentOfPinnedCoresAndNamesThePartThatIsBusy(t *testing.T) {
	text := partText(map[string]bench.PartCPUSummary{
		"postgres": {VCPUs: 6, MeanCores: 2.1, MeanPctOfPinned: 35, MaxPctOfPinned: 60},
		"api":      {VCPUs: 4, MeanCores: 3.5, MeanPctOfPinned: 87.5, MaxPctOfPinned: 99},
		"bench":    {MeanCores: 1.2}, // no core plan: shown in cores, never as a made-up percent
	})
	for _, want := range []string{"pg 35% (max 60%)", "api 88% (max 99%)", "gen 1.2 cores"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if partText(nil) != "" {
		t.Fatal("no parts must give no text")
	}
}
