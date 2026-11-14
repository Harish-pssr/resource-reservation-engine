package bench

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A ceiling is only an explanation if it can be attributed. Host CPU says the machine was 28% busy; it
// does not say whether the database, the API or the load generator was the part that ran out. PartSampler
// reads how much CPU each part really used, from the operating system (per process or per container
// cgroup), once per interval, so a result can say which part was near its cores' limit.

// Part is one thing whose CPU use is read: a process or a container.
type Part struct {
	Name  string
	VCPUs float64                            // how many vCPUs it may use (from the core plan); 0 when unknown
	Read  func() (cpuMicros uint64, ok bool) // cumulative CPU time; false when it cannot be read
}

// PartCPUSummary is what one part used over a run.
type PartCPUSummary struct {
	VCPUs           float64 `json:"pinned_vcpus,omitempty"`
	MeanCores       float64 `json:"mean_cores_busy"`
	MaxCores        float64 `json:"max_cores_busy"` // the busiest one-interval window
	MeanPctOfPinned float64 `json:"mean_pct_of_pinned,omitempty"`
	MaxPctOfPinned  float64 `json:"max_pct_of_pinned,omitempty"`
	Samples         int     `json:"samples"`
}

// PartSampler samples every Part until stopped.
type PartSampler struct {
	parts    []Part
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	acc      map[string]*partAcc
}

type partAcc struct {
	sumDelta float64 // microseconds of CPU
	sumWall  float64 // microseconds of wall time
	maxCores float64
	n        int
}

// NewPartSampler returns a sampler for the given parts.
func NewPartSampler(parts []Part, interval time.Duration) *PartSampler {
	return &PartSampler{parts: parts, interval: interval, done: make(chan struct{}), acc: map[string]*partAcc{}}
}

// Start begins sampling in the background.
func (s *PartSampler) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	go func() {
		defer close(s.done)
		w := newWindows(s.parts, time.Now(), s.interval)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			w.observe(time.Now(), s.add)
		}
	}()
}

// windows turns cumulative CPU counters into per-window readings.
type windows struct {
	parts    []Part
	min      time.Duration // a window shorter than this is merged into the next one
	prev     []uint64
	have     []bool
	lastTick time.Time
}

func newWindows(parts []Part, now time.Time, interval time.Duration) *windows {
	w := &windows{parts: parts, min: interval / 2, prev: make([]uint64, len(parts)), have: make([]bool, len(parts)), lastTick: now}
	for i, p := range parts {
		w.prev[i], w.have[i] = p.Read()
	}
	return w
}

// observe reads every part and reports the CPU used since the previous window. A sampler that was starved
// (a saturated process) can receive two ticks back to back: a few milliseconds of CPU measured over a window
// of one millisecond would look like a part using many times its cores, so a window shorter than half the
// interval is not reported on its own but merged into the next one. A counter that went back means the part
// restarted: that window is skipped.
func (w *windows) observe(now time.Time, report func(name string, cpuMicros, wallMicros float64)) {
	wall := now.Sub(w.lastTick)
	if wall < w.min {
		return // keep the previous reading: the next window spans both
	}
	for i, p := range w.parts {
		cur, ok := p.Read()
		if ok && w.have[i] && cur >= w.prev[i] {
			report(p.Name, float64(cur-w.prev[i]), float64(wall.Microseconds()))
		}
		w.prev[i], w.have[i] = cur, ok
	}
	w.lastTick = now
}

func (s *PartSampler) add(name string, deltaMicros, wallMicros float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acc[name]
	if a == nil {
		a = &partAcc{}
		s.acc[name] = a
	}
	a.sumDelta += deltaMicros
	a.sumWall += wallMicros
	a.maxCores = max(a.maxCores, deltaMicros/wallMicros)
	a.n++
}

// Stop ends sampling and returns the summary per part. A part that could never be read is absent: it is
// never filled in with a guess.
func (s *PartSampler) Stop() map[string]PartCPUSummary {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]PartCPUSummary{}
	for _, p := range s.parts {
		a := s.acc[p.Name]
		if a == nil || a.sumWall == 0 {
			continue
		}
		sum := PartCPUSummary{VCPUs: p.VCPUs, MeanCores: a.sumDelta / a.sumWall, MaxCores: a.maxCores, Samples: a.n}
		if p.VCPUs > 0 {
			sum.MeanPctOfPinned, sum.MaxPctOfPinned = sum.MeanCores/p.VCPUs*100, sum.MaxCores/p.VCPUs*100
		}
		out[p.Name] = sum
	}
	return out
}

// ---- readers (Linux) ----

// clockTicksPerSecond is USER_HZ, which is 100 on every Linux the benchmark runs on.
const clockTicksPerSecond = 100

// ParseProcStatCPU returns utime+stime in microseconds from the text of /proc/<pid>/stat. The command
// name sits in parentheses and may contain spaces, so the fields are counted after the last ')'.
func ParseProcStatCPU(stat string) (uint64, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:]) // f[0] is the state, field 3 of the file
	if len(f) < 13 {
		return 0, false
	}
	ut, err1 := strconv.ParseUint(f[11], 10, 64) // field 14
	st, err2 := strconv.ParseUint(f[12], 10, 64) // field 15
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return (ut + st) * (1_000_000 / clockTicksPerSecond), true
}

// ParseCgroupCPUStat returns usage_usec from the text of a cgroup v2 cpu.stat file.
func ParseCgroupCPUStat(stat string) (uint64, bool) {
	for _, line := range strings.Split(stat, "\n") {
		if rest, ok := strings.CutPrefix(line, "usage_usec "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// ParseCgroupPath returns the cgroup v2 path from the text of /proc/<pid>/cgroup ("0::/system.slice/x.scope").
func ParseCgroupPath(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// ProcessCPU reads one process's CPU time. The pid is read on every call so a caller can follow a
// process that is restarted under a new pid.
func ProcessCPU(pid func() int) func() (uint64, bool) {
	return func() (uint64, bool) {
		if runtime.GOOS != "linux" {
			return 0, false
		}
		p := pid()
		if p <= 0 {
			return 0, false
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(p) + "/stat")
		if err != nil {
			return 0, false
		}
		return ParseProcStatCPU(string(b))
	}
}

// SelfCPU reads this process's own CPU time (the load generator side).
func SelfCPU() func() (uint64, bool) { return ProcessCPU(os.Getpid) }

// CgroupCPU reads a cgroup's CPU time, for example a container's.
func CgroupCPU(cgroupDir string) func() (uint64, bool) {
	return func() (uint64, bool) {
		b, err := os.ReadFile(cgroupDir + "/cpu.stat")
		if err != nil {
			return 0, false
		}
		return ParseCgroupCPUStat(string(b))
	}
}

// ContainerCgroupDir finds the cgroup directory of a docker compose service, or "" when it cannot (not
// Linux, no docker, cgroup v1, service not running). It is looked up once per session: a container that
// is restarted keeps its cgroup, and a counter that goes back is skipped by the sampler.
func ContainerCgroupDir(ctx context.Context, service string) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	id, err := exec.CommandContext(ctx, "docker", "compose", "ps", "-q", service).Output()
	if err != nil || strings.TrimSpace(string(id)) == "" {
		return ""
	}
	pid, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Pid}}", strings.TrimSpace(string(id))).Output()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile("/proc/" + strings.TrimSpace(string(pid)) + "/cgroup")
	if err != nil {
		return ""
	}
	rel, ok := ParseCgroupPath(string(b))
	if !ok || rel == "/" || rel == "" {
		return ""
	}
	return "/sys/fs/cgroup" + rel
}
