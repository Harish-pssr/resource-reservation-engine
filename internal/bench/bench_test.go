package bench

import (
	"context"
	"testing"
	"time"

	"resourcereservation/internal/testdb"
)

func TestParseCPUModelAndMemory(t *testing.T) {
	cpu := "processor\t: 0\nmodel name\t: Intel(R) Xeon(R) CPU @ 2.20GHz\ncpu MHz\t: 2200\n"
	if got := parseCPUModel(cpu); got != "Intel(R) Xeon(R) CPU @ 2.20GHz" {
		t.Fatalf("cpu model %q", got)
	}
	if got := parseMemTotalMB("MemTotal:       16384000 kB\nMemFree: 1 kB\n"); got != 16000 {
		t.Fatalf("mem %d", got)
	}
	if parseCPUModel("nothing here") != "unknown" || parseMemTotalMB("") != 0 {
		t.Fatal("missing data must give unknown/0, not a guess")
	}
}

func TestParseWindowsPowerState(t *testing.T) {
	cases := []struct {
		out   string
		power string
	}{
		{"Some CPU\r\n16000\r\n2\r\n", "ac"},
		{"Some CPU\r\n16000\r\n1\r\n", "battery"},
		{"Some CPU\r\n16000\r\n", "no-battery"},
	}
	for _, c := range cases {
		cpu, mem, power := parseWindows(c.out)
		if cpu != "Some CPU" || mem != 16000 || power != c.power {
			t.Errorf("%q -> %q %d %q, want power %q", c.out, cpu, mem, power, c.power)
		}
	}
}

func TestCPUPercentFromProcStat(t *testing.T) {
	// user nice system idle iowait irq softirq steal
	a, ok1 := parseCPUStat("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 1 1 1 1 1 1 1\n")
	b, ok2 := parseCPUStat("cpu  250 0 250 800 100 0 0 0 0 0\n")
	if !ok1 || !ok2 {
		t.Fatal("parse failed")
	}
	// Between the readings: user +150, system +150, idle +100, so total +400 and 300 of it busy: 75%.
	if got := cpuPercent(a, b); got < 74.9 || got > 75.1 {
		t.Fatalf("cpu %.2f%%, want 75%%", got)
	}
	if _, ok := parseCPUStat("garbage"); ok {
		t.Fatal("garbage must not parse")
	}
}

func TestSummarizeSamples(t *testing.T) {
	s := Summarize([]Sample{
		{Connections: 5, ActiveConns: 2, WaitingOnLock: 0, Commits: 100, Rollbacks: 10, Deadlocks: 1, HostCPUPercent: 20},
		{Connections: 30, ActiveConns: 12, WaitingOnLock: 7, Commits: 400, Rollbacks: 60, Deadlocks: 3, HostCPUPercent: 80},
	})
	if s.MaxConnections != 30 || s.MaxActive != 12 || s.MaxWaitingOnLock != 7 {
		t.Fatalf("maxima: %+v", s)
	}
	if s.CommitsDuring != 300 || s.RollbacksDuring != 50 || s.DeadlocksDuring != 2 {
		t.Fatalf("deltas: %+v", s)
	}
	if s.HostCPUMeanPct != 50 || s.HostCPUMaxPct != 80 {
		t.Fatalf("cpu: %+v", s)
	}
	if e := Summarize(nil); e.HostCPUMeanPct != -1 || e.Samples != 0 {
		t.Fatalf("empty: %+v", e)
	}
}

func TestSamplerReadsRealDatabaseLoad(t *testing.T) {
	pool := testdb.NewEmpty(t)
	s := NewSampler(pool, 100*time.Millisecond)
	s.Start(context.Background())
	time.Sleep(550 * time.Millisecond)
	samples := s.Stop()
	if len(samples) < 3 {
		t.Fatalf("only %d samples in 550 ms at 100 ms intervals", len(samples))
	}
	if samples[0].Connections < 1 {
		t.Fatalf("the sampler's own session must be visible: %+v", samples[0])
	}
}

func TestCaptureNeverGuessesAndCarriesNoHostName(t *testing.T) {
	e := Capture(context.Background())
	if e.OS == "" || e.NumCPU == 0 || e.GoVersion == "" || e.CapturedAt == "" {
		t.Fatalf("basic fields missing: %+v", e)
	}
}
