package payment

import (
	"context"
	"testing"
	"time"
)

func tiny() Config {
	return Config{DelayMin: time.Millisecond, DelayMax: 3 * time.Millisecond, FailPct: 10, TimeoutPct: 5, Timeout: 20 * time.Millisecond}
}

func TestOutcomeBands(t *testing.T) {
	// roll is the first random draw (0..1, scaled by 100 inside Pay): under TimeoutPct -> timeout, next FailPct -> fail, else success.
	for _, tc := range []struct {
		roll float64
		want Result
	}{{0.02, TimedOut}, {0.07, Failed}, {0.5, Succeeded}} {
		first := true
		p := New(tiny()).WithRand(func() float64 {
			if first {
				first = false
				return tc.roll
			}
			return 0.5
		})
		got, err := p.Pay(context.Background())
		if err != nil || got != tc.want {
			t.Fatalf("roll %.2f: got %v, %v; want %v", tc.roll, got, err, tc.want)
		}
	}
}

func TestTimeoutWaitsForTheTimeout(t *testing.T) {
	p := New(tiny()).WithRand(func() float64 { return 0 }) // always the timeout band
	start := time.Now()
	got, _ := p.Pay(context.Background())
	if got != TimedOut || time.Since(start) < 20*time.Millisecond {
		t.Fatalf("a timeout must take the full timeout: %v after %v", got, time.Since(start))
	}
}

func TestCancelledContextStopsPayment(t *testing.T) {
	cfg := tiny()
	cfg.DelayMin, cfg.DelayMax = time.Hour, time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := New(cfg).WithRand(func() float64 { return 0.5 }).Pay(ctx); err == nil {
		t.Fatal("expected the context error")
	}
}

func TestProfiles(t *testing.T) {
	if f, err := Profile("fast"); err != nil || f.Timeout != time.Second {
		t.Fatalf("fast: %+v %v", f, err)
	}
	if r, err := Profile("realistic"); err != nil || r.DelayMax != 10*time.Second || r.Timeout != 15*time.Second {
		t.Fatalf("realistic: %+v %v", r, err)
	}
	if _, err := Profile("nope"); err == nil {
		t.Fatal("unknown profile must be an error")
	}
}
