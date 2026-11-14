package bench

import (
	"context"
	"os"
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

func TestParseAllowedCPUs(t *testing.T) {
	if got := parseAllowedCPUs("Name:\tbench\nCpus_allowed:\tffff\nCpus_allowed_list:\t10-13\nMems_allowed:\t1\n"); got != "10-13" {
		t.Fatalf("got %q", got)
	}
	if got := parseAllowedCPUs("Name:\tx\n"); got != "" {
		t.Fatalf("a missing line must give an empty answer, got %q", got)
	}
}

func TestStealPercent(t *testing.T) {
	// user nice system idle iowait irq softirq steal: total goes 1000 -> 2000, steal 10 -> 60 = 5%
	pct, ok := stealPercent("cpu  300 0 300 300 50 0 0 10 0 0\n", "cpu  600 0 600 600 100 0 0 60 0 0\n")
	if !ok || pct < 4.9 || pct > 5.1 {
		t.Fatalf("steal %.2f%% ok=%v, want 5%%", pct, ok)
	}
	for name, c := range map[string][2]string{
		"no time passed":    {"cpu  1 0 1 1 0 0 0 0\n", "cpu  1 0 1 1 0 0 0 0\n"},
		"counter went back": {"cpu  9 0 9 9 0 0 0 5\n", "cpu  10 0 10 10 0 0 0 1\n"},
		"garbage":           {"nope", "cpu  1 0 1 1 0 0 0 0\n"},
		"short line":        {"cpu  1 2 3\n", "cpu  4 5 6\n"},
	} {
		if _, ok := stealPercent(c[0], c[1]); ok {
			t.Fatalf("%s must not give a figure", name)
		}
	}
}

func TestCaptureRecordsPinningAndMachineTypeFromTheEnvironment(t *testing.T) {
	t.Setenv("BENCH_PINNING", "postgres=0-5 api=6-9 bench=10-13 redis=14 spare=15")
	t.Setenv("BENCH_MACHINE_TYPE", "n2-standard-16")
	e := Capture(context.Background())
	if e.Pinning != "postgres=0-5 api=6-9 bench=10-13 redis=14 spare=15" || e.MachineType != "n2-standard-16" {
		t.Fatalf("pinning/machine not recorded: %+v", e)
	}
	t.Setenv("BENCH_PINNING", "")
	t.Setenv("BENCH_MACHINE_TYPE", "")
	if e := Capture(context.Background()); e.Pinning != "" || e.MachineType != "" {
		t.Fatalf("unset values must stay empty, never guessed: %+v", e)
	}
}

func TestCountProcessors(t *testing.T) {
	info := "processor\t: 0\nmodel name\t: X\n\nprocessor\t: 1\nmodel name\t: X\n\nprocessor\t: 2\nprocessors in this line do not count: 9\n"
	if got := countProcessors(info); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
	if got := countProcessors("nothing here"); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func TestCaptureUsesTheCommitFromTheArchiveWhenThereIsNoRepository(t *testing.T) {
	dir := t.TempDir() // not a git repository
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Skip("cannot change directory")
	}
	defer os.Chdir(old)
	t.Setenv("BENCH_GIT_COMMIT", "abc1234")
	e := Capture(context.Background())
	if e.GitCommit != "abc1234" || e.GitDirty {
		t.Fatalf("commit %q dirty %v, want abc1234 and not dirty", e.GitCommit, e.GitDirty)
	}
	t.Setenv("BENCH_GIT_COMMIT", "")
	if e := Capture(context.Background()); e.GitCommit != "" {
		t.Fatalf("no repository and no archive commit must stay empty, got %q", e.GitCommit)
	}
}
