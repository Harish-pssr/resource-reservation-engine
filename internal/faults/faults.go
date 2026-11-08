// Package faults implements fault injection (D13): named hooks in the code whose behaviour is set by
// rows in the fault_config table. Hooks are no-ops unless FAULTS_ENABLED is true, and every method is
// safe on a nil *Registry, so components work unchanged when no registry is configured.
//
// A crash is a real process exit (os.Exit(137), no deferred cleanup), never a flag. A limited fault is
// consumed with one atomic UPDATE, so across any number of processes it fires at most `remaining` times
// and does not fire again after a restart. If the settings cannot be read, nothing fires: faults fail safe.
package faults

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Action is what a fault does when it fires.
type Action string

const (
	Crash   Action = "crash"   // exit the process abruptly (os.Exit(137))
	Error   Action = "error"   // the step returns ErrInjected
	Delay   Action = "delay"   // the step sleeps params.delay_ms
	Disable Action = "disable" // a state fault: switch a protection off while the row exists (negative controls)
)

// Known lists every injection point and the actions it supports. The control endpoints reject
// anything else, so a typo cannot silently do nothing. The points match docs/crash-analysis.md.
var Known = map[string][]Action{
	"hold.after_gate_claim":   {Crash, Error, Delay}, // row 6: after the Redis claim, before the database reserve
	"hold.before_commit":      {Crash, Error, Delay}, // row 8: inside the reserve transaction, before commit
	"hold.before_reply":       {Crash, Error, Delay}, // row 9: after the commit, before the reply
	"hold.before_gate_extend": {Crash, Error, Delay}, // row 10: after the commit, before the gate claim is extended
	"confirm.after_marker":    {Crash, Error, Delay}, // row 12: after the payment marker commits, before the payment
	"confirm.during_payment":  {Crash, Error, Delay}, // row 13: in the middle of the simulated payment
	"confirm.after_payment":   {Crash, Error, Delay}, // row 15: after the payment succeeded, before the confirm
	"confirm.before_commit":   {Crash, Error, Delay}, // row 16: inside the confirm transaction, before commit
	"confirm.before_reply":    {Crash, Error, Delay}, // row 17: after the confirm commit, before the reply
	"gate.sold_marker":        {Error},               // row 18: writing the sold marker fails
	"payment.delay":           {Delay},               // slows the simulated payment (rows 12-15)
	// A state fault: the payment marker is not checked or set, so a retry during a payment starts a
	// second payment. It exists only as the negative control for the marker (D5).
	"confirm.skip_payment_marker": {Disable},
}

// ErrInjected is the error returned by the Error action.
type ErrInjected struct{ Point string }

func (e ErrInjected) Error() string { return "injected fault at " + e.Point }

// Fault is one row of fault_config.
type Fault struct {
	Point       string         `json:"point"`
	Action      Action         `json:"action"`
	Params      map[string]any `json:"params,omitempty"`
	Remaining   *int           `json:"remaining"` // nil = unlimited
	Probability float64        `json:"probability"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// Validate checks the point, action and numeric ranges.
func (f Fault) Validate() error {
	actions, ok := Known[f.Point]
	if !ok {
		return fmt.Errorf("unknown injection point %q", f.Point)
	}
	if !slices.Contains(actions, f.Action) {
		return fmt.Errorf("point %q does not support action %q (supported: %v)", f.Point, f.Action, actions)
	}
	if f.Probability <= 0 || f.Probability > 1 {
		return errors.New("probability must be in (0, 1]")
	}
	if f.Remaining != nil && *f.Remaining < 0 {
		return errors.New("remaining must be zero or more")
	}
	return nil
}

// Registry caches fault_config and answers the hooks' questions.
type Registry struct {
	enabled  bool
	pool     *pgxpool.Pool
	interval time.Duration
	log      *slog.Logger
	cache    atomic.Pointer[map[string]Fault]
	rnd      func() float64
	exit     func(int) // replaceable so tests do not kill the test runner
}

// New returns a Registry. With enabled=false every hook is a no-op.
func New(pool *pgxpool.Pool, enabled bool, interval time.Duration, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	r := &Registry{enabled: enabled, pool: pool, interval: interval, log: log, rnd: rand.Float64, exit: os.Exit}
	empty := map[string]Fault{}
	r.cache.Store(&empty)
	return r
}

// Enabled reports whether fault injection is switched on for this process.
func (r *Registry) Enabled() bool { return r != nil && r.enabled }

// SetExit replaces the function a crash calls (tests only).
func (r *Registry) SetExit(fn func(int)) { r.exit = fn }

// SetRand replaces the random source (tests only).
func (r *Registry) SetRand(fn func() float64) { r.rnd = fn }

// Run refreshes the cache every interval until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) {
	if !r.Enabled() {
		return
	}
	for {
		if err := r.Refresh(ctx); err != nil && ctx.Err() == nil {
			// Keep serving the last known settings if the database is unreachable.
			r.log.Warn("fault refresh failed; keeping the last known faults", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.interval):
		}
	}
}

// Refresh reloads fault_config into the cache.
func (r *Registry) Refresh(ctx context.Context) error {
	list, err := r.List(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]Fault, len(list))
	for _, f := range list {
		m[f.Point] = f
	}
	r.cache.Store(&m)
	return nil
}

// Active returns the fault configured at point without consuming it. It serves state faults such as
// Disable, which stay in force while the row exists.
func (r *Registry) Active(point string) (Fault, bool) {
	if !r.Enabled() {
		return Fault{}, false
	}
	f, ok := (*r.cache.Load())[point]
	if !ok || (f.Remaining != nil && *f.Remaining == 0) {
		return Fault{}, false
	}
	return f, true
}

// Fire asks whether the fault at point should happen now. A limited fault is consumed with one atomic
// UPDATE, so across any number of processes it fires at most `remaining` times. Any error means "do
// not fire".
func (r *Registry) Fire(ctx context.Context, point string) (Fault, bool) {
	f, ok := r.Active(point)
	if !ok {
		return Fault{}, false
	}
	if f.Probability < 1 && r.rnd() >= f.Probability {
		return Fault{}, false
	}
	if f.Remaining != nil {
		var left int
		err := r.pool.QueryRow(ctx,
			`UPDATE fault_config SET remaining = remaining - 1 WHERE point = $1 AND remaining > 0 RETURNING remaining`,
			point).Scan(&left)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				r.log.Warn("could not consume fault", "point", point, "error", err)
			}
			return Fault{}, false
		}
	}
	r.log.Warn("fault fired", "point", point, "action", f.Action)
	return f, true
}

// Hit is the call a hook makes. It fires the fault at point, if any, and carries out its action: a
// crash exits the process and does not return; an error is returned; a delay sleeps (and returns
// early if ctx ends). With no fault set, or faults disabled, it returns nil at once.
func (r *Registry) Hit(ctx context.Context, point string) error {
	f, ok := r.Fire(ctx, point)
	if !ok {
		return nil
	}
	switch f.Action {
	case Crash:
		r.Crash()
	case Error:
		return ErrInjected{Point: point}
	case Delay:
		ms, _ := f.Params["delay_ms"].(float64)
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return nil
}

// Crash terminates the process the way a kill would: no deferred cleanup runs.
func (r *Registry) Crash() {
	r.log.Error("injected crash: exiting now", "status", "crash")
	r.exit(137)
}

// Set creates or replaces the fault at f.Point.
func (r *Registry) Set(ctx context.Context, f Fault) error {
	if f.Probability == 0 {
		f.Probability = 1
	}
	if err := f.Validate(); err != nil {
		return err
	}
	params := []byte("{}")
	if f.Params != nil {
		var err error
		if params, err = json.Marshal(f.Params); err != nil {
			return err
		}
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO fault_config (point, action, params, remaining, probability)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (point) DO UPDATE
		SET action = $2, params = $3, remaining = $4, probability = $5, updated_at = clock_timestamp()`,
		f.Point, string(f.Action), params, f.Remaining, f.Probability)
	return err
}

// List returns every configured fault.
func (r *Registry) List(ctx context.Context) ([]Fault, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT point, action, params, remaining, probability, updated_at FROM fault_config ORDER BY point`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Fault{}
	for rows.Next() {
		var f Fault
		var action string
		var params []byte
		if err := rows.Scan(&f.Point, &action, &params, &f.Remaining, &f.Probability, &f.UpdatedAt); err != nil {
			return nil, err
		}
		f.Action = Action(action)
		_ = json.Unmarshal(params, &f.Params)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Delete removes the fault at point (a no-op if there is none).
func (r *Registry) Delete(ctx context.Context, point string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM fault_config WHERE point = $1`, point)
	return err
}

// Clear removes every fault.
func (r *Registry) Clear(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM fault_config`)
	return err
}
