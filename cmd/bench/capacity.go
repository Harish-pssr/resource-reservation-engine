package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"resourcereservation/internal/bench"
	"resourcereservation/internal/loadgen"
	"resourcereservation/internal/payment"
)

// session is what every step of an E2 or E3 invocation shares.
type session struct {
	exp     string
	h       *harness
	pc      payment.Config
	profile string
	ttl     string
	seed    uint64
}

// stepSpec is one load step: a strategy at an arrival rate for a duration.
type stepSpec struct {
	Spec     strategySpec
	Skew     float64
	Rate     float64 // buyers per second
	Duration time.Duration
	Items    int     // 0 = sized from the rate (D10)
	RoomRate float64 // 0 = no waiting room
	Phase    string
	Label    string
	Seed     uint64
}

// step runs one step through the same harness as E1: fresh API process, reset store, fresh event,
// open-loop load, then the checker and the reconciler on the database.
func (s *session) step(ctx context.Context, sp stepSpec) (*record, error) {
	buyers := int(math.Round(sp.Rate * sp.Duration.Seconds()))
	items := sp.Items
	if items == 0 { // D10: large enough that the tail of the inventory cannot run out
		items = max(1000, int(math.Ceil(float64(buyers)*2.6*1.5)))
	}
	p := runParams{Experiment: s.exp, Strategy: sp.Spec.Name, APIStrategy: sp.Spec.Strategy, Schema: sp.Spec.Schema,
		Items: items, Buyers: buyers, ArrivalWindowS: sp.Duration.Seconds(), Skew: sp.Skew, Seed: sp.Seed,
		PaymentProfile: s.profile, HoldTTL: s.ttl, DBPoolSize: 32, Scope: scopeNote,
		RetryPolicy: fmt.Sprintf("lock_busy only, max %d retries, base %v x2 full jitter, cap %v",
			loadgen.DefaultRetry.MaxRetries, loadgen.DefaultRetry.Base, loadgen.DefaultRetry.Cap),
		Phase: sp.Phase, Label: sp.Label, RoomRate: sp.RoomRate}
	p.Payment.DelayMinMs, p.Payment.DelayMaxMs = s.pc.DelayMin.Milliseconds(), s.pc.DelayMax.Milliseconds()
	p.Payment.FailPct, p.Payment.TimeoutPct, p.Payment.TimeoutMs = s.pc.FailPct, s.pc.TimeoutPct, s.pc.Timeout.Milliseconds()
	return s.h.oneRun(ctx, p, s.pc)
}

// stepResult is the condensed outcome of one step, kept in the capacity result.
type stepResult struct {
	Rate           float64 `json:"rate_buyers_per_s"`
	Sustained      bool    `json:"sustained"`
	Reason         string  `json:"reason,omitempty"`
	HoldP50Ms      float64 `json:"hold_p50_ms"`
	HoldP99Ms      float64 `json:"hold_p99_ms_judged"` // the latency the D12 bound applies to
	BookingsPerSec float64 `json:"bookings_per_s_arrival_window"`
	HostCPUMeanPct float64 `json:"host_cpu_mean_percent"`
	Verdict        string  `json:"run_verdict"`
	DoubleSold     int     `json:"double_sold_items"`
	Record         string  `json:"record"`
}

func condense(r *record, label string) stepResult {
	return stepResult{Rate: r.Client.TargetRate, Sustained: r.Sustain.Sustained, Reason: r.Sustain.Reason,
		HoldP50Ms: bench.HoldP50Ms(r.Client), HoldP99Ms: bench.HoldP99Ms(r.Client), BookingsPerSec: r.Client.BookingsPerSecArrival,
		HostCPUMeanPct: r.StoreLoad.HostCPUMeanPct, Verdict: r.Verdict, DoubleSold: r.Checker.DoubleSoldItems, Record: "capacity-" + label + ".json"}
}

func (s *session) show(phase string, r *record) {
	verdict := "sustained"
	if !r.Sustain.Sustained {
		verdict = "NOT sustained: " + r.Sustain.Reason
	}
	fmt.Printf("  [%-8s] %6.0f buyers/s  %-44s hold p50 %6.1f ms, p99 %7.1f ms | %6.1f bookings/s | host CPU %s | checker %s\n",
		phase, r.Client.TargetRate, verdict, bench.HoldP50Ms(r.Client), bench.HoldP99Ms(r.Client), r.Client.BookingsPerSecArrival,
		cpuText(r.StoreLoad.HostCPUMeanPct), map[bool]string{true: "ok", false: "VIOLATION"}[r.Checker.OK()])
}

func cpuText(p float64) string {
	if p < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", p)
}

// protoConfig carries the D12 protocol numbers (tunable while developing, fixed for the final runs).
type protoConfig struct {
	Start, Factor, Max    float64
	RampStep, ConfirmStep time.Duration
	LatencyRuns           int
}

type confirmation struct {
	Rate    float64      `json:"rate_buyers_per_s"`
	Passed  bool         `json:"majority_sustained"`
	Reps    []stepResult `json:"reps"`
	Outcome []bool       `json:"outcomes"`
}

// capacityResult is everything the protocol measured for one configuration.
type capacityResult struct {
	Strategy      string         `json:"strategy"`
	Skew          float64        `json:"skew_zipf_exponent"`
	RoomRate      float64        `json:"room_rate_buyers_per_s"`
	Bound         float64        `json:"hold_p99_bound_ms"`
	Ramp          []stepResult   `json:"ramp"`
	Confirmations []confirmation `json:"confirmations"`
	Confirmed     float64        `json:"confirmed_capacity_buyers_per_s"` // 0 = none
	// LadderExhausted: the top of the ladder was still sustained, so Confirmed is only a lower bound.
	LadderExhausted bool              `json:"ladder_exhausted_capacity_is_a_lower_bound"`
	LatencyRate     float64           `json:"latency_rate_buyers_per_s"` // 70% of the confirmed capacity
	Latency         []stepResult      `json:"latency_runs"`
	Environment     bench.Environment `json:"environment"`
}

// protocol is D12: a geometric ramp to the first failing rate, confirmation of the best rate by majority
// vote walking up or down, then latency at 70% of the confirmed capacity.
func (s *session) protocol(ctx context.Context, spec strategySpec, skew, roomRate float64, pc protoConfig) (*capacityResult, error) {
	res := &capacityResult{Strategy: spec.Name, Skew: skew, RoomRate: roomRate, Bound: bench.HoldP99BoundMs, Environment: s.h.env}
	full := bench.Ladder(pc.Start, pc.Factor, pc.Max)
	tag := fmt.Sprintf("%s-skew%g", spec.Name, skew)
	seedFor := func(rate float64, rep int) uint64 { return s.seed + uint64(rate)*131 + uint64(rep) }
	run := func(rate float64, dur time.Duration, phase string, rep int) (*record, string, error) {
		label := fmt.Sprintf("%s-%s-%g-rep%d", tag, phase, rate, rep)
		r, err := s.step(ctx, stepSpec{Spec: spec, Skew: skew, Rate: rate, Duration: dur, RoomRate: roomRate, Phase: phase, Label: label, Seed: seedFor(rate, rep)})
		if err == nil {
			s.show(phase, r)
		}
		return r, label, err
	}

	fmt.Printf("\n== %s, skew %g%s: ramp from %g buyers/s, factor %g, %v steps\n", spec.Name, skew,
		map[bool]string{true: fmt.Sprintf(", room R=%g/s", roomRate), false: ""}[roomRate > 0], pc.Start, pc.Factor, pc.RampStep)
	best := -1
	for i, rate := range full {
		r, label, err := run(rate, pc.RampStep, "ramp", 0)
		if err != nil {
			return nil, err
		}
		res.Ramp = append(res.Ramp, condense(r, label))
		if !r.Sustain.Sustained {
			break
		}
		best = i
	}
	if best < 0 {
		fmt.Println("  no rate in the ladder was sustained: capacity is below the first step")
		return res, s.saveCapacity(res, tag)
	}

	fmt.Printf("  confirming from %g buyers/s (majority of up to 3 runs of %v)\n", full[best], pc.ConfirmStep)
	var firstErr error
	idx := bench.ConfirmCapacity(len(full), best, func(i int) bool {
		if firstErr != nil {
			return false
		}
		c := confirmation{Rate: full[i]}
		c.Passed, c.Outcome, firstErr = bench.Majority(func(rep int) (bool, error) {
			r, label, err := run(full[i], pc.ConfirmStep, "confirm", rep)
			if err != nil {
				return false, err
			}
			c.Reps = append(c.Reps, condense(r, label))
			return r.Sustain.Sustained, nil
		})
		res.Confirmations = append(res.Confirmations, c)
		return c.Passed
	})
	if firstErr != nil {
		return nil, firstErr
	}
	if idx < 0 {
		fmt.Println("  no rate was confirmed")
		return res, s.saveCapacity(res, tag)
	}
	res.Confirmed = full[idx]
	res.LadderExhausted = idx == len(full)-1
	fmt.Printf("  confirmed capacity: %g buyers/s (hold p99 under %.0f ms, no unknown outcomes)\n", res.Confirmed, bench.HoldP99BoundMs)
	if res.LadderExhausted {
		fmt.Printf("  WARNING: %g is the top of the ladder and was still sustained: it is a LOWER BOUND, not the capacity. Raise -max-rate.\n", res.Confirmed)
	}

	res.LatencyRate = math.Round(0.7 * res.Confirmed)
	for rep := 1; rep <= pc.LatencyRuns && res.LatencyRate >= 1; rep++ {
		r, label, err := run(res.LatencyRate, pc.ConfirmStep, "latency", rep)
		if err != nil {
			return nil, err
		}
		res.Latency = append(res.Latency, condense(r, label))
	}
	return res, s.saveCapacity(res, tag)
}

func (s *session) saveCapacity(res *capacityResult, tag string) error {
	if err := os.MkdirAll(s.h.out, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	return os.WriteFile(filepath.Join(s.h.out, fmt.Sprintf("%s-%s-capacity.json", s.exp, tag)), b, 0o644)
}

// ---- shared flags ----

type common struct {
	apiBin, out, profile, holdTTL string
	start, factor, max            float64
	rampStep, confirmStep         time.Duration
	seed                          uint64
	allowBattery, apiLogs         bool
}

func addCommon(fs *flag.FlagSet, defaultOut string) *common {
	c := &common{}
	fs.StringVar(&c.apiBin, "api", defaultAPIBin(), "path to the API binary (scripts/build builds it)")
	fs.StringVar(&c.out, "out", defaultOut, "directory for the raw records")
	fs.StringVar(&c.profile, "profile", "realistic", "payment profile: fast or realistic")
	fs.StringVar(&c.holdTTL, "hold-ttl", "", "hold time (default 300s for realistic, 30s for fast)")
	fs.Float64Var(&c.start, "start", 50, "first rate of the ramp, buyers per second")
	fs.Float64Var(&c.factor, "factor", 1.25, "ramp step factor")
	fs.Float64Var(&c.max, "max-rate", 4000, "highest rate to try, buyers per second")
	fs.DurationVar(&c.rampStep, "ramp-step", 20*time.Second, "length of a ramp step")
	fs.DurationVar(&c.confirmStep, "confirm-step", 30*time.Second, "length of a confirmation or latency run")
	fs.Uint64Var(&c.seed, "seed", 1, "base seed (strategies see the same demand at the same rate)")
	fs.BoolVar(&c.allowBattery, "allow-battery", false, "run even on battery power (results are then not publishable)")
	fs.BoolVar(&c.apiLogs, "api-logs", false, "show the API's own log output")
	return c
}

func (c *common) open(ctx context.Context, exp string) (*session, error) {
	pc, err := payment.Profile(c.profile)
	if err != nil {
		return nil, err
	}
	ttl := c.holdTTL
	if ttl == "" {
		ttl = "300s"
		if c.profile == "fast" {
			ttl = "30s"
		}
	}
	env := bench.Capture(ctx)
	if env.PowerState == "battery" && !c.allowBattery {
		return nil, fmt.Errorf("the machine is on battery power; plug in, or pass -allow-battery (then the results are not publishable)")
	}
	fmt.Printf("environment: %s %s, %d CPUs, %s, %d MB, power %s, commit %s (dirty=%v)\n",
		env.OS, env.Arch, env.NumCPU, env.CPUModel, env.MemTotalMB, env.PowerState, env.GitCommit, env.GitDirty)
	if runtime.GOOS == "windows" {
		fmt.Println("NOTE: Windows runs are for development only; final numbers come from the Linux VM.")
	}
	h := &harness{apiBin: c.apiBin, dbURL: defaultDB(), redis: defaultRedis(), out: c.out, env: env, keepLogs: c.apiLogs}
	return &session{exp: exp, h: h, pc: pc, profile: c.profile, ttl: ttl, seed: c.seed}, nil
}

func (c *common) proto() protoConfig {
	return protoConfig{Start: c.start, Factor: c.factor, Max: c.max, RampStep: c.rampStep, ConfirmStep: c.confirmStep, LatencyRuns: 3}
}

func pickStrategies(list string) ([]strategySpec, error) {
	var out []strategySpec
	for _, name := range strings.Split(list, ",") {
		found := false
		for _, s := range allStrategies {
			if s.Name == strings.TrimSpace(name) {
				out, found = append(out, s), true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown strategy %q", name)
		}
	}
	return out, nil
}

func parseFloats(list string) ([]float64, error) {
	var out []float64
	for _, f := range strings.Split(list, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q", f)
		}
		out = append(out, v)
	}
	return out, nil
}

// ---- E2 ----

// driftReport compares the baseline run at the start of a session with the one at the end (drift control).
type driftReport struct {
	Strategy   string     `json:"strategy"`
	Rate       float64    `json:"rate_buyers_per_s"`
	First      stepResult `json:"first"`
	Last       stepResult `json:"last"`
	P50DriftPc float64    `json:"hold_p50_drift_percent"`
	P99DriftPc float64    `json:"hold_p99_drift_percent"`
	BkgDriftPc float64    `json:"bookings_per_s_drift_percent"`
}

func pct(first, last float64) float64 {
	if first == 0 {
		return 0
	}
	return (last - first) / first * 100
}

// e2 measures capacity and latency per strategy with the D12 protocol, with the baseline
// configuration run first and last so drift in the machine is visible.
func e2(args []string) error {
	fs := flag.NewFlagSet("e2", flag.ExitOnError)
	c := addCommon(fs, "benchmark-scratch/e2")
	strategies := fs.String("strategies", "locking,optimistic,fastpath", "comma-separated strategies")
	skews := fs.String("skews", "1.1", "comma-separated Zipf exponents; the protocol runs once per strategy and skew")
	baselineRate := fs.Float64("baseline-rate", 100, "buyers per second of the drift-control baseline run")
	warmup := fs.Duration("warmup", 10*time.Second, "length of the warm-up run before the first baseline (recorded, never used in a comparison); 0 disables")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	specs, err := pickStrategies(*strategies)
	if err != nil {
		return err
	}
	skewList, err := parseFloats(*skews)
	if err != nil {
		return err
	}
	s, err := c.open(ctx, "e2")
	if err != nil {
		return err
	}
	baseline := func(which string) (*record, error) {
		fmt.Printf("\n== baseline %s: locking at %g buyers/s\n", which, *baselineRate)
		r, err := s.step(ctx, stepSpec{Spec: allStrategies[2], Skew: skewList[0], Rate: *baselineRate, Duration: c.confirmStep,
			Phase: "baseline-" + which, Label: "baseline-" + which, Seed: s.seed})
		if err == nil {
			s.show("baseline", r)
		}
		return r, err
	}

	if *warmup > 0 { // a cold API, pool and page cache would otherwise be mistaken for drift
		fmt.Printf("\n== warm-up: locking at %g buyers/s for %v (recorded, not used)\n", *baselineRate, *warmup)
		w, err := s.step(ctx, stepSpec{Spec: allStrategies[2], Skew: skewList[0], Rate: *baselineRate, Duration: *warmup,
			Phase: "warmup", Label: "warmup", Seed: s.seed})
		if err != nil {
			return err
		}
		s.show("warmup", w)
	}
	first, err := baseline("first")
	if err != nil {
		return err
	}
	var results []*capacityResult
	for _, skew := range skewList {
		for _, spec := range specs {
			res, err := s.protocol(ctx, spec, skew, 0, c.proto())
			if err != nil {
				return fmt.Errorf("%s skew %g: %w", spec.Name, skew, err)
			}
			results = append(results, res)
		}
	}
	last, err := baseline("last")
	if err != nil {
		return err
	}

	d := driftReport{Strategy: "locking", Rate: *baselineRate, First: condense(first, "baseline-first"), Last: condense(last, "baseline-last"),
		P50DriftPc: pct(bench.HoldP50Ms(first.Client), bench.HoldP50Ms(last.Client)), P99DriftPc: pct(bench.HoldP99Ms(first.Client), bench.HoldP99Ms(last.Client)),
		BkgDriftPc: pct(first.Client.BookingsPerSecArrival, last.Client.BookingsPerSecArrival)}
	b, _ := json.MarshalIndent(d, "", "  ")
	os.WriteFile(filepath.Join(c.out, "e2-baseline-drift.json"), b, 0o644)

	fmt.Printf("\nE2 summary (hold p99 bound %.0f ms; profile %s; one machine)\n", bench.HoldP99BoundMs, c.profile)
	fmt.Printf("%-12s %5s | %12s | %8s %8s | %s\n", "strategy", "skew", "capacity/s", "p50 @70%", "p99 @70%", "bookings/s @70%")
	for _, r := range results {
		if r.Confirmed == 0 || len(r.Latency) == 0 {
			fmt.Printf("%-12s %5g | %12s |\n", r.Strategy, r.Skew, "none")
			continue
		}
		var p50, p99, bk []float64
		for _, l := range r.Latency {
			p50, p99, bk = append(p50, l.HoldP50Ms), append(p99, l.HoldP99Ms), append(bk, l.BookingsPerSec)
		}
		bound := ""
		if r.LadderExhausted {
			bound = " (LOWER BOUND: ladder exhausted)"
		}
		fmt.Printf("%-12s %5g | %12.0f | %8.1f %8.1f | %.1f (medians of %d runs)%s\n", r.Strategy, r.Skew, r.Confirmed, median(p50), median(p99), median(bk), len(r.Latency), bound)
	}
	fmt.Printf("\ndrift (baseline last vs first): hold p50 %+.1f%%, p99 %+.1f%%, bookings/s %+.1f%%\nraw records in %s\n", d.P50DriftPc, d.P99DriftPc, d.BkgDriftPc, c.out)
	return nil
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	switch n := len(s); {
	case n == 0:
		return 0
	case n%2 == 1:
		return s[n/2]
	default:
		return (s[n/2-1] + s[n/2]) / 2
	}
}
