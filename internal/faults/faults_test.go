package faults

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"resourcereservation/internal/testdb"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func intp(n int) *int { return &n }

func armed(t *testing.T) (*Registry, context.Context) {
	t.Helper()
	pool := testdb.New(t, true)
	r := New(pool, true, time.Hour, discard())
	return r, context.Background()
}

func TestNilAndDisabledRegistriesNeverFire(t *testing.T) {
	var nilReg *Registry
	if err := nilReg.Hit(context.Background(), "hold.before_commit"); err != nil {
		t.Fatalf("a nil registry must be a no-op: %v", err)
	}
	if nilReg.Enabled() {
		t.Fatal("a nil registry is not enabled")
	}
	r, ctx := armed(t)
	disabled := New(r.pool, false, time.Hour, discard())
	if err := r.Set(ctx, Fault{Point: "hold.before_commit", Action: Error}); err != nil {
		t.Fatal(err)
	}
	disabled.Refresh(ctx)
	if err := disabled.Hit(ctx, "hold.before_commit"); err != nil {
		t.Fatalf("a disabled registry must ignore the table: %v", err)
	}
}

func TestSetRejectsUnknownPointsAndBadValues(t *testing.T) {
	r, ctx := armed(t)
	bad := []Fault{
		{Point: "no.such.point", Action: Crash},
		{Point: "gate.sold_marker", Action: Crash},                   // that point only supports error
		{Point: "hold.before_commit", Action: Error, Probability: 2}, // probability out of range
		{Point: "hold.before_commit", Action: Error, Remaining: intp(-1)},
	}
	for _, f := range bad {
		if err := r.Set(ctx, f); err == nil {
			t.Errorf("%+v must be rejected", f)
		}
	}
	if err := r.Set(ctx, Fault{Point: "hold.before_commit", Action: Crash, Remaining: intp(1)}); err != nil {
		t.Fatalf("a valid fault must be accepted: %v", err)
	}
}

func TestErrorActionReturnsAnInjectedError(t *testing.T) {
	r, ctx := armed(t)
	r.Set(ctx, Fault{Point: "confirm.before_commit", Action: Error})
	r.Refresh(ctx)
	err := r.Hit(ctx, "confirm.before_commit")
	var inj ErrInjected
	if !errors.As(err, &inj) || inj.Point != "confirm.before_commit" {
		t.Fatalf("got %v", err)
	}
	if err := r.Hit(ctx, "hold.before_commit"); err != nil {
		t.Fatalf("an unarmed point must not fire: %v", err)
	}
}

func TestDelayActionSleeps(t *testing.T) {
	r, ctx := armed(t)
	r.Set(ctx, Fault{Point: "payment.delay", Action: Delay, Params: map[string]any{"delay_ms": 120}})
	r.Refresh(ctx)
	start := time.Now()
	if err := r.Hit(ctx, "payment.delay"); err != nil || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("a delay fault must sleep: %v after %v", err, time.Since(start))
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	start = time.Now()
	r.Hit(short, "payment.delay")
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("a delay must stop when the request context ends")
	}
}

// A crash exits the process with status 137 and no cleanup; here the exit is intercepted.
func TestCrashActionExitsWithStatus137(t *testing.T) {
	r, ctx := armed(t)
	var status atomic.Int32
	r.SetExit(func(code int) { status.Store(int32(code)) })
	r.Set(ctx, Fault{Point: "hold.before_reply", Action: Crash})
	r.Refresh(ctx)
	r.Hit(ctx, "hold.before_reply")
	if status.Load() != 137 {
		t.Fatalf("exit status %d, want 137", status.Load())
	}
}

// "Crash once" must fire exactly once across ALL processes: the count is consumed by an atomic UPDATE.
func TestOneShotFaultFiresExactlyOnceAcrossProcesses(t *testing.T) {
	pool := testdb.New(t, true)
	ctx := context.Background()
	procs := []*Registry{New(pool, true, time.Hour, discard()), New(pool, true, time.Hour, discard()), New(pool, true, time.Hour, discard())}
	if err := procs[0].Set(ctx, Fault{Point: "hold.before_reply", Action: Error, Remaining: intp(3)}); err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		p.Refresh(ctx)
	}
	var fired atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 90; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if procs[i%3].Hit(ctx, "hold.before_reply") != nil {
				fired.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if fired.Load() != 3 {
		t.Fatalf("fired %d times, want exactly the 3 that were allowed, across three processes", fired.Load())
	}
	// It stays consumed: a restarted process (a fresh registry) reads remaining = 0 and does not fire.
	restarted := New(pool, true, time.Hour, discard())
	restarted.Refresh(ctx)
	if restarted.Hit(ctx, "hold.before_reply") != nil {
		t.Fatal("a consumed fault must not fire again after a restart")
	}
}

func TestProbabilityGatesTheFiring(t *testing.T) {
	r, ctx := armed(t)
	r.Set(ctx, Fault{Point: "hold.before_commit", Action: Error, Probability: 0.25})
	r.Refresh(ctx)
	r.SetRand(func() float64 { return 0.9 }) // above the probability: skip
	if r.Hit(ctx, "hold.before_commit") != nil {
		t.Fatal("must not fire when the draw is above the probability")
	}
	r.SetRand(func() float64 { return 0.1 })
	if r.Hit(ctx, "hold.before_commit") == nil {
		t.Fatal("must fire when the draw is below the probability")
	}
}

// Faults fail safe: if the table cannot be read or consumed, nothing fires.
func TestFaultsFailSafeWhenTheDatabaseIsUnreachable(t *testing.T) {
	pool := testdb.New(t, true)
	ctx := context.Background()
	r := New(pool, true, time.Hour, discard())
	r.Set(ctx, Fault{Point: "hold.before_commit", Action: Error, Remaining: intp(1)})
	r.Refresh(ctx)
	pool.Close() // the database goes away after the settings were cached
	if err := r.Hit(ctx, "hold.before_commit"); err != nil {
		t.Fatalf("a fault that cannot be consumed must not fire: %v", err)
	}
	if err := r.Refresh(ctx); err == nil {
		t.Fatal("refresh must report the failure")
	}
	if _, ok := r.Active("hold.before_commit"); !ok {
		t.Fatal("the last known settings must stay in force when a refresh fails")
	}
}

func TestDisableIsAStateFaultThatStaysInForce(t *testing.T) {
	r, ctx := armed(t)
	if _, on := r.Active("confirm.skip_payment_marker"); on {
		t.Fatal("not set yet")
	}
	r.Set(ctx, Fault{Point: "confirm.skip_payment_marker", Action: Disable})
	r.Refresh(ctx)
	for i := 0; i < 3; i++ { // reading a state fault never consumes it
		if _, on := r.Active("confirm.skip_payment_marker"); !on {
			t.Fatal("a state fault stays in force")
		}
	}
	r.Delete(ctx, "confirm.skip_payment_marker")
	r.Refresh(ctx)
	if _, on := r.Active("confirm.skip_payment_marker"); on {
		t.Fatal("deleting the row switches the fault off")
	}
}

func TestListAndClear(t *testing.T) {
	r, ctx := armed(t)
	r.Set(ctx, Fault{Point: "hold.before_commit", Action: Crash, Remaining: intp(1)})
	r.Set(ctx, Fault{Point: "payment.delay", Action: Delay, Params: map[string]any{"delay_ms": 5}})
	list, err := r.List(ctx)
	if err != nil || len(list) != 2 || list[0].Point != "hold.before_commit" || list[0].Remaining == nil || *list[0].Remaining != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	r.Clear(ctx)
	if list, _ := r.List(ctx); len(list) != 0 {
		t.Fatalf("clear left %d", len(list))
	}
}
