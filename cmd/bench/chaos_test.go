package main

import (
	"slices"
	"testing"
	"time"

	"resourcereservation/internal/faults"
)

func count(events []chaosEvent, k chaosKind) (n int) {
	for _, e := range events {
		if e.Kind == k {
			n++
		}
	}
	return
}

func TestScheduleHasTheRequestedEventsInsideTheWindow(t *testing.T) {
	window := 30 * time.Second
	ev := buildSchedule(2, 3, 1, 1, window, true, 7)
	if len(ev) != 7 || count(ev, kSigkill) != 2 || count(ev, kHook) != 3 || count(ev, kRedis) != 1 || count(ev, kPostgres) != 1 {
		t.Fatalf("%+v", ev)
	}
	for _, e := range ev {
		if e.At < time.Duration(0.15*float64(window)) || e.At > time.Duration(0.80*float64(window)) {
			t.Errorf("%v at %v is outside the middle of the window", e.Kind, e.At)
		}
	}
	if !slices.IsSortedFunc(ev, func(a, b chaosEvent) int { return int(a.At - b.At) }) {
		t.Fatal("events must be in time order")
	}
}

// Each failure must be recovered from before the next one: events are spaced out when the window allows.
func TestScheduleSpacesEventsSoTheSystemRecoversBetweenThem(t *testing.T) {
	for seed := uint64(1); seed <= 50; seed++ {
		ev := buildSchedule(2, 2, 0, 0, 60*time.Second, false, seed)
		for i := 1; i < len(ev); i++ {
			if gap := ev[i].At - ev[i-1].At; gap < minGap/2 {
				t.Fatalf("seed %d: events %d and %d are only %v apart", seed, i-1, i, gap)
			}
		}
	}
}

func TestScheduleIsDeterministicInTheSeed(t *testing.T) {
	a := buildSchedule(2, 2, 1, 1, 30*time.Second, true, 11)
	b := buildSchedule(2, 2, 1, 1, 30*time.Second, true, 11)
	if !slices.Equal(a, b) {
		t.Fatal("the same seed must give the same schedule")
	}
	if slices.Equal(a, buildSchedule(2, 2, 1, 1, 30*time.Second, true, 12)) {
		t.Fatal("a different seed must give a different schedule")
	}
}

// Every armed point must be a real hook, and the fast-path points must appear only with the fast path.
func TestScheduleArmsOnlyKnownCrashPointsAndFastPathOnesOnlyWhenRelevant(t *testing.T) {
	for _, p := range append(append([]string{}, hookPoints...), fastpathHookPoints...) {
		acts, ok := faults.Known[p]
		if !ok || !slices.Contains(acts, faults.Crash) {
			t.Errorf("%s is not a known point that supports a crash", p)
		}
	}
	for seed := uint64(1); seed <= 100; seed++ {
		for _, e := range buildSchedule(0, 4, 0, 0, 40*time.Second, false, seed) {
			if slices.Contains(fastpathHookPoints, e.Point) {
				t.Fatalf("seed %d armed %s without the fast path", seed, e.Point)
			}
		}
	}
	seen := map[string]bool{}
	for seed := uint64(1); seed <= 200; seed++ {
		for _, e := range buildSchedule(0, 4, 0, 0, 40*time.Second, true, seed) {
			seen[e.Point] = true
		}
	}
	for _, p := range append(append([]string{}, hookPoints...), fastpathHookPoints...) {
		if !seen[p] {
			t.Errorf("point %s is never chosen", p)
		}
	}
}

func TestEmptyPlanHasNoEvents(t *testing.T) {
	if ev := buildSchedule(0, 0, 0, 0, 30*time.Second, true, 1); ev != nil {
		t.Fatalf("%+v", ev)
	}
}
