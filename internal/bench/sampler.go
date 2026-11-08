package bench

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Sample is one reading of how loaded the store was. Counters (Commits, Rollbacks, Deadlocks) are
// cumulative since the database was created; subtract the first sample to get the run's share.
type Sample struct {
	TS             float64 `json:"t_s"`             // seconds since the sampler started
	Connections    int     `json:"connections"`     // all sessions on this database
	ActiveConns    int     `json:"active"`          // sessions executing a query
	WaitingOnLock  int     `json:"waiting_on_lock"` // sessions blocked waiting for a row or table lock
	Commits        int64   `json:"commits"`
	Rollbacks      int64   `json:"rollbacks"`
	Deadlocks      int64   `json:"deadlocks"`
	HostCPUPercent float64 `json:"host_cpu_percent"` // whole machine, Linux only; -1 where unavailable
}

// Sampler reads database load once per interval until stopped. Host CPU is a whole-machine figure:
// it stands for the store's CPU only when Postgres runs on this machine, and it includes the API.
type Sampler struct {
	pool     *pgxpool.Pool
	interval time.Duration
	mu       sync.Mutex
	samples  []Sample
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewSampler returns a sampler on its own small connection use (one query pair per interval).
func NewSampler(pool *pgxpool.Pool, interval time.Duration) *Sampler {
	return &Sampler{pool: pool, interval: interval, done: make(chan struct{})}
}

// Start begins sampling in the background.
func (s *Sampler) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	start := time.Now()
	go func() {
		defer close(s.done)
		var prev cpuTimes
		prev, _ = readCPUTimes()
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			smp := Sample{TS: time.Since(start).Seconds(), HostCPUPercent: -1}
			if cur, ok := readCPUTimes(); ok {
				smp.HostCPUPercent = cpuPercent(prev, cur)
				prev = cur
			}
			err := s.pool.QueryRow(ctx, `
				SELECT count(*), count(*) FILTER (WHERE state = 'active'),
				       count(*) FILTER (WHERE wait_event_type = 'Lock')
				FROM pg_stat_activity WHERE datname = current_database()`).
				Scan(&smp.Connections, &smp.ActiveConns, &smp.WaitingOnLock)
			if err != nil {
				continue
			}
			_ = s.pool.QueryRow(ctx, `
				SELECT xact_commit, xact_rollback, deadlocks FROM pg_stat_database WHERE datname = current_database()`).
				Scan(&smp.Commits, &smp.Rollbacks, &smp.Deadlocks)
			s.mu.Lock()
			s.samples = append(s.samples, smp)
			s.mu.Unlock()
		}
	}()
}

// Stop ends sampling and returns every sample taken.
func (s *Sampler) Stop() []Sample {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.samples...)
}

// LoadSummary condenses a sample series for a result table.
type LoadSummary struct {
	Samples          int     `json:"samples"`
	MaxConnections   int     `json:"max_connections"`
	MaxActive        int     `json:"max_active"`
	MaxWaitingOnLock int     `json:"max_waiting_on_lock"`
	DeadlocksDuring  int64   `json:"deadlocks_during_run"`
	RollbacksDuring  int64   `json:"rollbacks_during_run"`
	CommitsDuring    int64   `json:"commits_during_run"`
	HostCPUMeanPct   float64 `json:"host_cpu_mean_percent"` // -1 if not measured
	HostCPUMaxPct    float64 `json:"host_cpu_max_percent"`
}

// Summarize condenses the samples.
func Summarize(samples []Sample) LoadSummary {
	l := LoadSummary{Samples: len(samples), HostCPUMeanPct: -1, HostCPUMaxPct: -1}
	if len(samples) == 0 {
		return l
	}
	var cpuSum float64
	var cpuN int
	for _, s := range samples {
		l.MaxConnections = max(l.MaxConnections, s.Connections)
		l.MaxActive = max(l.MaxActive, s.ActiveConns)
		l.MaxWaitingOnLock = max(l.MaxWaitingOnLock, s.WaitingOnLock)
		if s.HostCPUPercent >= 0 {
			cpuSum += s.HostCPUPercent
			cpuN++
			l.HostCPUMaxPct = max(l.HostCPUMaxPct, s.HostCPUPercent)
		}
	}
	if cpuN > 0 {
		l.HostCPUMeanPct = cpuSum / float64(cpuN)
	}
	first, last := samples[0], samples[len(samples)-1]
	l.DeadlocksDuring, l.RollbacksDuring, l.CommitsDuring = last.Deadlocks-first.Deadlocks, last.Rollbacks-first.Rollbacks, last.Commits-first.Commits
	return l
}

// cpuTimes is the aggregate line of /proc/stat.
type cpuTimes struct{ idle, total uint64 }

func readCPUTimes() (cpuTimes, bool) {
	if runtime.GOOS != "linux" {
		return cpuTimes{}, false
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	return parseCPUStat(string(b))
}

// parseCPUStat reads the first line of /proc/stat: "cpu user nice system idle iowait irq softirq steal ...".
func parseCPUStat(stat string) (cpuTimes, bool) {
	line, _, _ := strings.Cut(stat, "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuTimes{}, false
	}
	var t cpuTimes
	for i, v := range f[1:] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		if i >= 8 { // guest and guest_nice are already counted in user and nice
			break
		}
		t.total += n
		if i == 3 || i == 4 { // idle and iowait
			t.idle += n
		}
	}
	return t, true
}

// cpuPercent is the share of non-idle time between two readings.
func cpuPercent(prev, cur cpuTimes) float64 {
	dTotal := float64(cur.total - prev.total)
	if dTotal <= 0 {
		return 0
	}
	return (1 - float64(cur.idle-prev.idle)/dTotal) * 100
}
