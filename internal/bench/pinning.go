package bench

import (
	"strconv"
	"strings"
)

// ParsePinning reads the core plan the scripts export in BENCH_PINNING ("postgres=0-2,8-10 api=3-4,11-12
// bench=5-6,13-14 redis=7 spare=15") and returns how many vCPUs each part may use. Anything that does not
// parse is left out rather than guessed.
func ParsePinning(plan string) map[string]float64 {
	out := map[string]float64{}
	for _, item := range strings.Fields(plan) {
		name, list, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if n, ok := countCPUList(list); ok {
			out[name] = float64(n)
		}
	}
	return out
}

// countCPUList counts the CPUs in a list such as "0-2,8-10" (6) or "7" (1).
func countCPUList(list string) (int, bool) {
	if list == "" {
		return 0, false
	}
	n := 0
	for _, part := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return 0, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return 0, false
			}
		}
		n += b - a + 1
	}
	return n, true
}
