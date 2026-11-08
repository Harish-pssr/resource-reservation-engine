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
	"time"
)

// surgeMode condenses one run of the surge experiment (with or without the room).
type surgeMode struct {
	Room           bool    `json:"waiting_room"`
	Sustained      bool    `json:"sustained"`
	Reason         string  `json:"not_sustained_reason,omitempty"`
	GeneratorOK    bool    `json:"generator_kept_up"`
	StoreHoldP50Ms float64 `json:"store_facing_hold_p50_ms"`
	StoreHoldP99Ms float64 `json:"store_facing_hold_p99_ms"`
	BuyerHoldP99Ms float64 `json:"buyer_facing_hold_p99_ms"` // from intended send time: includes any queue wait
	QueueWaitP99Ms float64 `json:"queue_wait_p99_ms"`
	Held           int     `json:"hold_held"`
	Unavailable    int     `json:"hold_unavailable"`
	LockBusy       int     `json:"hold_lock_busy_after_retries"`
	TurnedAway     int     `json:"room_turned_away"`
	GaveUp         int     `json:"room_gave_up"`
	Admitted       int     `json:"room_admitted"`
	UnknownOutcome int     `json:"unknown_outcomes"`
	Confirmed      int     `json:"confirmed"`
	FIFOInversions int     `json:"fifo_inversions"`
	HostCPUMeanPct float64 `json:"host_cpu_mean_percent"`
	HostCPUMaxPct  float64 `json:"host_cpu_max_percent"`
	MaxConnections int     `json:"store_max_connections"`
	MaxActive      int     `json:"store_max_active"`
	MaxLockWaiters int     `json:"store_max_waiting_on_lock"`
	Rollbacks      int64   `json:"store_rollbacks_during_run"`
	Deadlocks      int64   `json:"store_deadlocks_during_run"`
	DoubleSold     int     `json:"double_sold_items"`
	Oversold       int     `json:"oversold_beyond_capacity"`
	ReconcileOK    bool    `json:"reconcile_ok"`
	Record         string  `json:"record"`
}

func surgeFrom(r *record, label string) surgeMode {
	c, l := r.Client, r.StoreLoad
	return surgeMode{Room: r.Params.RoomRate > 0, Sustained: r.Sustain.Sustained, Reason: r.Sustain.Reason, GeneratorOK: c.GeneratorOK,
		StoreHoldP50Ms: c.StoreHoldP50Ms, StoreHoldP99Ms: c.StoreHoldP99Ms, BuyerHoldP99Ms: c.HoldP99Ms, QueueWaitP99Ms: c.QueueWaitP99Ms,
		Held: c.HoldHeld, Unavailable: c.HoldUnavailable, LockBusy: c.HoldBusy, TurnedAway: c.RoomTurnedAway, GaveUp: c.RoomGaveUp,
		Admitted: c.RoomAdmitted, UnknownOutcome: c.HoldErrors + c.ConfirmErrors + c.RoomErrors, Confirmed: c.Confirmed,
		FIFOInversions: c.FIFOInversions, HostCPUMeanPct: l.HostCPUMeanPct, HostCPUMaxPct: l.HostCPUMaxPct,
		MaxConnections: l.MaxConnections, MaxActive: l.MaxActive, MaxLockWaiters: l.MaxWaitingOnLock,
		Rollbacks: l.RollbacksDuring, Deadlocks: l.DeadlocksDuring, DoubleSold: r.Checker.DoubleSoldItems, Oversold: r.Checker.Oversold,
		ReconcileOK: r.Reconcile.OK(), Record: "e3-" + label + ".json"}
}

type surgeFactor struct {
	Factor    float64   `json:"oversubscription_factor"` // items requested / items available (D10), ignoring skew
	OfferedPS float64   `json:"offered_buyers_per_s"`
	Items     int       `json:"items"`
	NoRoom    surgeMode `json:"without_room"`
	WithRoom  surgeMode `json:"with_room"`
}

type surgeResult struct {
	Strategy       string        `json:"strategy"`
	Skew           float64       `json:"skew_zipf_exponent"`
	CapacityPS     float64       `json:"capacity_buyers_per_s"`
	CapacitySource string        `json:"capacity_source"`
	RoomRatePS     float64       `json:"room_rate_buyers_per_s"` // R = share x capacity
	RoomShare      float64       `json:"room_share_of_capacity"`
	StepSeconds    float64       `json:"step_seconds"`
	Factors        []surgeFactor `json:"factors"`
}

// e3 measures what the waiting room does to the store under surge. In one session, on one machine,
// it first finds the confirmed capacity C of the strategy WITHOUT the room (D12), sets R = 0.8 x C
// (D9 addendum), then offers f x C buyers per second for several f, with and without the room.
func e3(args []string) error {
	fs := flag.NewFlagSet("e3", flag.ExitOnError)
	c := addCommon(fs, "benchmark-scratch/e3")
	strategy := fs.String("strategy", "fastpath", "the strategy to protect (use the best one found by e2)")
	factors := fs.String("factors", "5,10,20", "oversubscription factors")
	duration := fs.Duration("duration", 20*time.Second, "length of each surge run")
	capacity := fs.Float64("capacity", 0, "skip the capacity protocol and use this confirmed capacity (buyers/s); recorded as NOT measured in this session")
	skew := fs.Float64("skew", 1.1, "Zipf exponent of demand")
	share := fs.Float64("room-share", 0.8, "R as a share of the confirmed capacity")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	specs, err := pickStrategies(*strategy)
	if err != nil || len(specs) != 1 {
		return fmt.Errorf("e3 takes exactly one strategy: %v", err)
	}
	spec := specs[0]
	fl, err := parseFloats(*factors)
	if err != nil {
		return err
	}
	s, err := c.open(ctx, "e3")
	if err != nil {
		return err
	}

	res := surgeResult{Strategy: spec.Name, Skew: *skew, RoomShare: *share, StepSeconds: duration.Seconds()}
	if *capacity > 0 {
		res.CapacityPS, res.CapacitySource = *capacity, "supplied by the operator, NOT measured in this session"
	} else {
		cap, err := s.protocol(ctx, spec, *skew, 0, c.proto())
		if err != nil {
			return err
		}
		if cap.Confirmed == 0 {
			return fmt.Errorf("no capacity could be confirmed for %s; cannot set the room rate", spec.Name)
		}
		res.CapacityPS, res.CapacitySource = cap.Confirmed, "measured in this session with the D12 protocol, without the waiting room"
	}
	res.RoomRatePS = math.Round(*share * res.CapacityPS)
	fmt.Printf("\n== surge: capacity C = %g buyers/s (%s)\n   waiting room rate R = %.1f x C = %g buyers/s\n", res.CapacityPS, res.CapacitySource, *share, res.RoomRatePS)

	for _, f := range fl {
		offered := math.Round(f * res.CapacityPS)
		items := int(math.Ceil(res.CapacityPS * duration.Seconds() * 2.6)) // items requested at 1x capacity: O = f
		sf := surgeFactor{Factor: f, OfferedPS: offered, Items: items}
		for _, roomRate := range []float64{0, res.RoomRatePS} {
			mode := map[bool]string{false: "no-room", true: "room"}[roomRate > 0]
			label := fmt.Sprintf("%s-x%g-%s", spec.Name, f, mode)
			fmt.Printf("\n-- %gx capacity (%g buyers/s offered), %s\n", f, offered, mode)
			r, err := s.step(ctx, stepSpec{Spec: spec, Skew: *skew, Rate: offered, Duration: *duration, Items: items, RoomRate: roomRate,
				Phase: "e3-" + mode, Label: label, Seed: s.seed + uint64(f)})
			if err != nil {
				return fmt.Errorf("%gx %s: %w", f, mode, err)
			}
			s.show("e3-"+mode, r)
			if roomRate > 0 {
				sf.WithRoom = surgeFrom(r, label)
			} else {
				sf.NoRoom = surgeFrom(r, label)
			}
		}
		res.Factors = append(res.Factors, sf)
	}

	b, _ := json.MarshalIndent(res, "", "  ")
	os.MkdirAll(c.out, 0o755)
	os.WriteFile(filepath.Join(c.out, fmt.Sprintf("e3-%s-surge.json", spec.Name)), b, 0o644)

	fmt.Printf("\nE3 summary: %s, skew %g, C = %g buyers/s, R = %g buyers/s, %v runs; profile %s; one machine\n", spec.Name, *skew, res.CapacityPS, res.RoomRatePS, *duration, c.profile)
	fmt.Printf("%-6s %-8s | %11s %11s | %8s %8s | %6s %6s %6s | %7s %7s | %9s | %s\n",
		"factor", "mode", "store p99ms", "buyer p99ms", "CPU mean", "CPU max", "conns", "active", "lockw", "held", "unavail", "turned/gaveup", "invariants")
	row := func(f float64, name string, m surgeMode) {
		inv := "ok"
		if m.DoubleSold > 0 || m.Oversold > 0 || !m.ReconcileOK {
			inv = "VIOLATION"
		}
		note := ""
		if !m.GeneratorOK {
			note = " (generator behind: not usable for latency)"
		}
		fmt.Printf("%-6g %-8s | %11.1f %11.1f | %8s %8s | %6d %6d %6d | %7d %7d | %5d/%-5d | %s, inversions %d%s\n", f, name,
			m.StoreHoldP99Ms, m.BuyerHoldP99Ms, cpuText(m.HostCPUMeanPct), cpuText(m.HostCPUMaxPct), m.MaxConnections, m.MaxActive,
			m.MaxLockWaiters, m.Held, m.Unavailable, m.TurnedAway, m.GaveUp, inv, m.FIFOInversions, note)
	}
	for _, sf := range res.Factors {
		row(sf.Factor, "no-room", sf.NoRoom)
		row(sf.Factor, "room", sf.WithRoom)
	}
	fmt.Printf("\nraw records in %s\n", c.out)
	return nil
}
