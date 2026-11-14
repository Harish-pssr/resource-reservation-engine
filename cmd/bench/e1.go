package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"resourcereservation/internal/bench"
	"resourcereservation/internal/loadgen"
	"resourcereservation/internal/payment"
)

// e1 is the oversell test (the headline): N buyers compete for K items under skewed demand, for
// every strategy, several runs each. Every run is reported; none is dropped.
func e1(args []string) error {
	fs := flag.NewFlagSet("e1", flag.ExitOnError)
	apiBin := fs.String("api", defaultAPIBin(), "path to the API binary (scripts/build builds it)")
	out := fs.String("out", "benchmark-scratch/e1", "directory for the raw records")
	items := fs.Int("items", 1000, "items in the event (K)")
	buyers := fs.Int("buyers", 10000, "virtual buyers (N), one reserve request each")
	arrival := fs.Duration("arrival", 10*time.Second, "window over which all buyers arrive")
	skew := fs.Float64("skew", 1.1, "Zipf exponent of demand")
	runs := fs.Int("runs", 5, "runs per strategy")
	profile := fs.String("profile", "realistic", "payment profile: fast or realistic")
	holdTTL := fs.String("hold-ttl", "", "hold time, e.g. 300s (default: 300s for the realistic profile, 30s for fast)")
	only := fs.String("strategies", "", "comma-separated subset of: naive-unprotected,naive-protected,locking,optimistic,fastpath")
	seed := fs.Uint64("seed", 1, "base seed; run i uses seed+i, the same for every strategy so they see the same demand")
	allowBattery := fs.Bool("allow-battery", false, "run even on battery power (results are then not publishable)")
	keepLogs := fs.Bool("api-logs", false, "show the API's own log output")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	pc, err := payment.Profile(*profile)
	if err != nil {
		return err
	}
	ttl := *holdTTL
	if ttl == "" {
		ttl = "300s"
		if *profile == "fast" {
			ttl = "30s"
		}
	}
	specs := allStrategies
	if *only != "" {
		specs = nil
		for _, name := range strings.Split(*only, ",") {
			found := false
			for _, s := range allStrategies {
				if s.Name == strings.TrimSpace(name) {
					specs, found = append(specs, s), true
				}
			}
			if !found {
				return fmt.Errorf("unknown strategy %q", name)
			}
		}
	}

	env := bench.Capture(ctx)
	if env.PowerState == "battery" && !*allowBattery {
		return fmt.Errorf("the machine is on battery power; plug in, or pass -allow-battery (then the results are not publishable)")
	}
	fmt.Printf("environment: %s %s, %d CPUs, %s, %d MB, power %s, commit %s (dirty=%v)\n",
		env.OS, env.Arch, env.NumCPU, env.CPUModel, env.MemTotalMB, env.PowerState, env.GitCommit, env.GitDirty)
	if runtime.GOOS == "windows" {
		fmt.Println("NOTE: Windows runs are for development only; final numbers come from the Linux VM.")
	}

	h := &harness{apiBin: *apiBin, dbURL: defaultDB(), redis: defaultRedis(), out: *out, env: env, keepLogs: *keepLogs}
	fmt.Printf("E1: %d buyers over %v for %d items, skew %.2f, payment %s, hold %s, %d runs x %d strategies\n\n",
		*buyers, *arrival, *items, *skew, *profile, ttl, *runs, len(specs))
	fmt.Printf("%-18s %3s | %5s %6s %6s | %9s %10s %9s | %-10s %s\n",
		"strategy", "run", "held", "booked", "items", "double-sd", "extra-bkgs", "oversold", "reconcile", "verdict")

	// Runs are interleaved by run number so slow drift in the machine affects every strategy alike.
	for run := 1; run <= *runs; run++ {
		for _, spec := range specs {
			p := runParams{Experiment: "e1", Strategy: spec.Name, APIStrategy: spec.Strategy, Schema: spec.Schema, Run: run,
				Items: *items, Buyers: *buyers, ArrivalWindowS: arrival.Seconds(), Skew: *skew, Seed: *seed + uint64(run),
				PaymentProfile: *profile, HoldTTL: ttl, DBPoolSize: poolSize(),
				RetryPolicy: fmt.Sprintf("lock_busy only, max %d retries, base %v x2 full jitter, cap %v",
					loadgen.DefaultRetry.MaxRetries, loadgen.DefaultRetry.Base, loadgen.DefaultRetry.Cap),
				Scope: scopeNote}
			p.Payment.DelayMinMs, p.Payment.DelayMaxMs = pc.DelayMin.Milliseconds(), pc.DelayMax.Milliseconds()
			p.Payment.FailPct, p.Payment.TimeoutPct, p.Payment.TimeoutMs = pc.FailPct, pc.TimeoutPct, pc.Timeout.Milliseconds()

			r, err := h.oneRun(ctx, p, pc)
			if err != nil {
				return fmt.Errorf("%s run %d: %w", spec.Name, run, err)
			}
			rec := "ok"
			if !r.Reconcile.OK() {
				rec = "MISMATCH"
			}
			fmt.Printf("%-18s %3d | %5d %6d %6d | %9d %10d %9d | %-10s %s\n", spec.Name, run, r.Client.HoldHeld, r.Client.Confirmed,
				r.Client.ConfirmedItems, r.Checker.DoubleSoldItems, r.Checker.ExtraBookings, r.Checker.Oversold, rec, r.Verdict)
		}
	}
	fmt.Printf("\nraw records in %s\n", *out)
	return nil
}

func defaultAPIBin() string {
	if runtime.GOOS == "windows" {
		return "bin/api.exe"
	}
	return "bin/api"
}
