package bench

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePinningCountsVCPUsPerPart(t *testing.T) {
	got := ParsePinning("postgres=0-2,8-10 api=3-4,11-12 bench=5-6,13-14 redis=7 spare=15")
	want := map[string]float64{"postgres": 6, "api": 4, "bench": 4, "redis": 1, "spare": 1}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	for _, bad := range []string{"", "nonsense", "api=", "api=3-", "api=5-3", "api=x"} {
		if len(ParsePinning(bad)) != 0 {
			t.Fatalf("%q must not parse, got %v", bad, ParsePinning(bad))
		}
	}
}

func TestParseProcStatCPUHandlesSpacesInTheCommandName(t *testing.T) {
	// fields after ")": state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime
	stat := "123 (my (odd) name) S 1 123 123 0 -1 4194560 100 0 0 0 250 150 0 0 20 0 1 0 100 1000 100"
	us, ok := ParseProcStatCPU(stat)
	if !ok || us != (250+150)*10_000 {
		t.Fatalf("got %d ok=%v, want %d", us, ok, (250+150)*10_000)
	}
	for _, bad := range []string{"", "no parentheses", "1 (x) S 1 2 3"} {
		if _, ok := ParseProcStatCPU(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

func TestParseCgroupFiles(t *testing.T) {
	if n, ok := ParseCgroupCPUStat("usage_usec 123456\nuser_usec 100000\nsystem_usec 23456\n"); !ok || n != 123456 {
		t.Fatalf("got %d ok=%v", n, ok)
	}
	if _, ok := ParseCgroupCPUStat("user_usec 1\n"); ok {
		t.Fatal("a file without usage_usec must not parse")
	}
	if p, ok := ParseCgroupPath("0::/system.slice/docker-abc.scope\n"); !ok || p != "/system.slice/docker-abc.scope" {
		t.Fatalf("got %q ok=%v", p, ok)
	}
	if _, ok := ParseCgroupPath("12:cpu:/old/v1/path\n"); ok {
		t.Fatal("a cgroup v1 line must not parse as v2")
	}
}

func TestPartSamplerReportsCoresBusyAndSkipsARestart(t *testing.T) {
	// A part that burns 2 cores' worth of CPU: 2,000 microseconds of CPU per 1,000 microseconds of wall time is
	// not what a ticker gives us, so the counter advances per read and the test checks the arithmetic on the
	// real wall-clock windows: cores = delta / wall.
	var counter atomic.Uint64
	busy := Part{Name: "busy", VCPUs: 4, Read: func() (uint64, bool) {
		time.Sleep(0)
		return counter.Add(40_000), true // 40 ms of CPU per read
	}}
	var calls atomic.Int64
	restarts := Part{Name: "restarts", Read: func() (uint64, bool) {
		n := calls.Add(1)
		if n == 4 {
			return 0, true // the counter went back: the part restarted
		}
		return uint64(n) * 10_000, true
	}}
	unreadable := Part{Name: "never", Read: func() (uint64, bool) { return 0, false }}

	s := NewPartSampler([]Part{busy, restarts, unreadable}, 20*time.Millisecond)
	s.Start(context.Background())
	time.Sleep(200 * time.Millisecond)
	got := s.Stop()

	if _, ok := got["never"]; ok {
		t.Fatalf("a part that cannot be read must be absent, got %+v", got["never"])
	}
	b, ok := got["busy"]
	if !ok || b.Samples < 4 {
		t.Fatalf("busy part missing or barely sampled: %+v", got)
	}
	// 40 ms of CPU per roughly 20 ms window is about 2 cores; allow a wide margin for scheduling jitter.
	if b.MeanCores < 0.5 || b.MeanCores > 8 || b.MaxCores < b.MeanCores {
		t.Fatalf("implausible cores busy: %+v", b)
	}
	if b.MeanPctOfPinned <= 0 || b.VCPUs != 4 {
		t.Fatalf("percent of the pinned vCPUs missing: %+v", b)
	}
	r := got["restarts"]
	if r.Samples == 0 {
		t.Fatalf("the part that restarted must still have samples from the other windows: %+v", r)
	}
	// 10,000 us of CPU per 20 ms window is 0.5 cores; a restart window must not have produced a huge value
	// from the counter going back (an unsigned subtraction would give about 1.8e19).
	if r.MaxCores > 5 {
		t.Fatalf("a restart must be skipped, got max %v cores", r.MaxCores)
	}
}

// A starved sampler gets two ticks almost together. CPU measured over such a sliver must not be reported as
// a part using many times its cores (this showed up on 2026-11-14 as a load generator "at 661%").
func TestWindowsMergeASliverIntoTheNextWindowInsteadOfReportingAnImpossibleRate(t *testing.T) {
	var counter uint64
	part := Part{Name: "busy", VCPUs: 1, Read: func() (uint64, bool) { return counter, true }}
	t0 := time.Unix(1000, 0)
	w := newWindows([]Part{part}, t0, time.Second)
	var rates []float64
	report := func(_ string, cpu, wall float64) { rates = append(rates, cpu/wall) }

	counter = 1_000_000 // one full second of CPU in the first second: 1 core
	w.observe(t0.Add(time.Second), report)
	counter += 10_000 // 10 ms of CPU, then a tick only 1 ms later: the sliver
	w.observe(t0.Add(time.Second+time.Millisecond), report)
	counter += 990_000 // the rest of that second: the next real tick spans both
	w.observe(t0.Add(2*time.Second), report)

	if len(rates) != 2 {
		t.Fatalf("the sliver must not be reported on its own: got %d windows %v", len(rates), rates)
	}
	for _, r := range rates {
		if r < 0.99 || r > 1.01 {
			t.Fatalf("a part using one core must read as one core, got %v", rates)
		}
	}
}
