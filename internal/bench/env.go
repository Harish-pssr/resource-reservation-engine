// Package bench holds the measurement tooling shared by the runners: environment capture, a
// store-load sampler, and the run record. Every result file embeds the environment it came from, so
// numbers from different machines are never mixed by accident.
package bench

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Environment describes where a run happened. It deliberately carries no host name, user name or
// address: result files are published.
type Environment struct {
	CapturedAt      string `json:"captured_at"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	NumCPU          int    `json:"num_cpu"` // the CPUs THIS process may use (Go respects pinning), not the machine size: see LogicalCPUs
	CPUModel        string `json:"cpu_model"`
	MemTotalMB      int    `json:"mem_total_mb"`
	PowerState      string `json:"power_state"` // ac, battery, no-battery, or unknown. Never benchmark on battery.
	GoVersion       string `json:"go_version"`
	PostgresVersion string `json:"postgres_version,omitempty"`
	RedisVersion    string `json:"redis_version,omitempty"`
	GitCommit       string `json:"git_commit,omitempty"`
	GitDirty        bool   `json:"git_dirty"` // uncommitted changes: the numbers may not match any commit

	// Where the parts ran. On a shared machine a figure means little without the core split.
	LogicalCPUs    int      `json:"logical_cpus,omitempty"` // CPUs the machine has (Linux), whatever this process is pinned to
	Kernel         string   `json:"kernel,omitempty"`
	MachineType    string   `json:"machine_type,omitempty"`    // from BENCH_MACHINE_TYPE: an instance type, never a host name
	BackendProfile string   `json:"backend_profile,omitempty"` // from BENCH_BACKEND_PROFILE: "full" or "small" (a deliberately constrained database)
	Pinning        string   `json:"pinning,omitempty"`         // from BENCH_PINNING, e.g. "postgres=0-5 api=6-9 bench=10-13 redis=14 spare=15"
	AllowedCPUs    string   `json:"allowed_cpus,omitempty"`    // the cores THIS process may run on (the load generator side)
	CPUStealPct    *float64 `json:"cpu_steal_pct,omitempty"`   // share of CPU time the hypervisor took, over a short window when the session started
}

// Capture reads the environment. Anything it cannot determine is left as "unknown" or empty rather
// than guessed.
func Capture(ctx context.Context) Environment {
	e := Environment{
		CapturedAt: time.Now().UTC().Format(time.RFC3339), OS: runtime.GOOS, Arch: runtime.GOARCH,
		NumCPU: runtime.NumCPU(), GoVersion: runtime.Version(), CPUModel: "unknown", PowerState: "unknown",
	}
	switch runtime.GOOS {
	case "linux":
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			e.CPUModel = parseCPUModel(string(b))
		}
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			e.MemTotalMB = parseMemTotalMB(string(b))
		}
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			e.LogicalCPUs = countProcessors(string(b))
		}
		e.PowerState = "no-battery" // a server VM
		if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			e.Kernel = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile("/proc/self/status"); err == nil {
			e.AllowedCPUs = parseAllowedCPUs(string(b))
		}
		e.CPUStealPct = measureSteal(ctx, 500*time.Millisecond)
	case "windows":
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_Processor | Select-Object -First 1 -ExpandProperty Name); "+
				"[math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory/1MB); "+
				"(Get-CimInstance Win32_Battery | Select-Object -First 1 -ExpandProperty BatteryStatus)").Output()
		if err == nil {
			e.CPUModel, e.MemTotalMB, e.PowerState = parseWindows(string(out))
		}
	}
	e.MachineType, e.Pinning = strings.TrimSpace(os.Getenv("BENCH_MACHINE_TYPE")), strings.TrimSpace(os.Getenv("BENCH_PINNING"))
	e.BackendProfile = strings.TrimSpace(os.Getenv("BENCH_BACKEND_PROFILE"))
	if out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		e.GitCommit = strings.TrimSpace(string(out))
		if st, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output(); err == nil {
			e.GitDirty = len(strings.TrimSpace(string(st))) > 0
		}
	} else if c := strings.TrimSpace(os.Getenv("BENCH_GIT_COMMIT")); c != "" {
		// A machine that received a clean archive of a commit has no repository: the scripts pass the commit the
		// archive was made from. Such a tree is the commit, unmodified, so it is not dirty.
		e.GitCommit = c
	}
	return e
}

func parseCPUModel(cpuinfo string) string {
	for _, line := range strings.Split(cpuinfo, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

func parseMemTotalMB(meminfo string) int {
	for _, line := range strings.Split(meminfo, "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				if kb, err := strconv.Atoi(f[0]); err == nil {
					return kb / 1024
				}
			}
		}
	}
	return 0
}

// parseWindows reads the three lines of the PowerShell query: CPU name, memory in MB, and the
// battery status code if a battery exists (2 = on AC power, 1 = discharging).
func parseWindows(out string) (cpu string, memMB int, power string) {
	lines := []string{}
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	cpu, power = "unknown", "no-battery"
	if len(lines) > 0 {
		cpu = lines[0]
	}
	if len(lines) > 1 {
		memMB, _ = strconv.Atoi(lines[1])
	}
	if len(lines) > 2 {
		switch lines[2] {
		case "1":
			power = "battery"
		case "2", "3", "4", "5", "6", "7", "8", "9":
			power = "ac" // charging, charged, or otherwise on mains
		default:
			power = "unknown"
		}
	}
	return cpu, memMB, power
}

// parseAllowedCPUs reads the "Cpus_allowed_list" line of /proc/<pid>/status ("6-9", "0-5,8").
func parseAllowedCPUs(status string) string {
	for _, line := range strings.Split(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// stealPercent is the share of CPU time taken by the hypervisor between two readings of the first
// line of /proc/stat ("cpu user nice system idle iowait irq softirq steal ..."). ok is false when
// either reading does not parse or no time passed.
func stealPercent(before, after string) (pct float64, ok bool) {
	read := func(stat string) (steal, total uint64, ok bool) {
		line, _, _ := strings.Cut(stat, "\n")
		f := strings.Fields(line)
		if len(f) < 9 || f[0] != "cpu" {
			return 0, 0, false
		}
		for i, v := range f[1:9] { // user nice system idle iowait irq softirq steal
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += n
			if i == 7 {
				steal = n
			}
		}
		return steal, total, true
	}
	s0, t0, ok0 := read(before)
	s1, t1, ok1 := read(after)
	if !ok0 || !ok1 || t1 <= t0 || s1 < s0 {
		return 0, false
	}
	return float64(s1-s0) / float64(t1-t0) * 100, true
}

// measureSteal samples /proc/stat twice, window apart. nil when it cannot be read: it is never guessed.
func measureSteal(ctx context.Context, window time.Duration) *float64 {
	a, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}
	select {
	case <-time.After(window):
	case <-ctx.Done():
		return nil
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}
	if pct, ok := stealPercent(string(a), string(b)); ok {
		return &pct
	}
	return nil
}

// countProcessors counts the "processor : n" entries of /proc/cpuinfo: the CPUs the machine has,
// unlike runtime.NumCPU, which follows the process's own affinity.
func countProcessors(cpuinfo string) int {
	n := 0
	for _, line := range strings.Split(cpuinfo, "\n") {
		if k, _, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "processor" {
			n++
		}
	}
	return n
}
