// Package payment simulates an external payment provider. It is a stand-in, not a real
// integration: every claim made about results holds "with a simulated payment step".
// The delay, failure rate and timeout are configurable so tests and benchmarks can name them.
package payment

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// Result is how one payment attempt ended.
type Result string

const (
	Succeeded Result = "succeeded"
	Failed    Result = "failed"
	TimedOut  Result = "timed_out" // the provider did not answer within Timeout; the real outcome is unknown
)

// Config describes the simulated provider.
type Config struct {
	DelayMin, DelayMax time.Duration // a normal attempt takes a uniform time in this range
	FailPct            float64       // share of attempts that fail (0..100)
	TimeoutPct         float64       // share of attempts that never answer in time (0..100)
	Timeout            time.Duration // how long the caller waits before giving up
}

// Named profiles (see the experiment parameters). Every result records the profile it used.
var (
	// Fast is for unit, integration and expiry-race tests.
	Fast = Config{DelayMin: 50 * time.Millisecond, DelayMax: 200 * time.Millisecond, FailPct: 10, TimeoutPct: 2, Timeout: time.Second}
	// Realistic is the default for benchmarks.
	Realistic = Config{DelayMin: 2 * time.Second, DelayMax: 10 * time.Second, FailPct: 10, TimeoutPct: 2, Timeout: 15 * time.Second}
)

// Profile returns a named profile.
func Profile(name string) (Config, error) {
	switch name {
	case "fast":
		return Fast, nil
	case "realistic":
		return Realistic, nil
	}
	return Config{}, fmt.Errorf("unknown payment profile %q (want fast or realistic)", name)
}

// Provider is the simulated payment service.
type Provider struct {
	cfg  Config
	rand func() float64 // uniform in [0,1); replaceable so tests are deterministic
}

// New returns a Provider.
func New(cfg Config) *Provider { return &Provider{cfg: cfg, rand: rand.Float64} }

// WithRand returns a copy that draws from r (tests only).
func (p *Provider) WithRand(r func() float64) *Provider { return &Provider{cfg: p.cfg, rand: r} }

// Config returns the configuration, so results can record it.
func (p *Provider) Config() Config { return p.cfg }

// Pay runs one simulated payment. It blocks for the simulated time, or until ctx ends.
func (p *Provider) Pay(ctx context.Context) (Result, error) { return p.PayWith(ctx, nil) }

// PayWith is Pay with a callback run halfway through the wait: the point where a process can die
// "during the payment" (D13). A callback error ends the payment with that error.
func (p *Provider) PayWith(ctx context.Context, midpoint func(context.Context) error) (Result, error) {
	roll := p.rand() * 100
	var wait time.Duration
	result := Succeeded
	switch {
	case roll < p.cfg.TimeoutPct:
		wait, result = p.cfg.Timeout, TimedOut
	default:
		span := p.cfg.DelayMax - p.cfg.DelayMin
		wait = p.cfg.DelayMin + time.Duration(p.rand()*float64(span))
		if roll < p.cfg.TimeoutPct+p.cfg.FailPct {
			result = Failed
		}
	}
	if midpoint != nil {
		select {
		case <-time.After(wait / 2):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if err := midpoint(ctx); err != nil {
			return "", err
		}
		wait -= wait / 2
	}
	select {
	case <-time.After(wait):
		return result, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
