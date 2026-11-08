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
	NumCPU          int    `json:"num_cpu"`
	CPUModel        string `json:"cpu_model"`
	MemTotalMB      int    `json:"mem_total_mb"`
	PowerState      string `json:"power_state"` // ac, battery, no-battery, or unknown. Never benchmark on battery.
	GoVersion       string `json:"go_version"`
	PostgresVersion string `json:"postgres_version,omitempty"`
	RedisVersion    string `json:"redis_version,omitempty"`
	GitCommit       string `json:"git_commit,omitempty"`
	GitDirty        bool   `json:"git_dirty"` // uncommitted changes: the numbers may not match any commit
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
		e.PowerState = "no-battery" // a server VM
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
	if out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		e.GitCommit = strings.TrimSpace(string(out))
		if st, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output(); err == nil {
			e.GitDirty = len(strings.TrimSpace(string(st))) > 0
		}
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
