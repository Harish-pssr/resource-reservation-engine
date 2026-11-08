package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"resourcereservation/internal/bench"
	"resourcereservation/internal/checker"
	"resourcereservation/internal/db"
	"resourcereservation/internal/loadgen"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reconcile"
	"resourcereservation/internal/workload"
)

// e4Plan is one failure experiment: what is broken, how often, and under what workload.
type e4Plan struct {
	Name       string   `json:"name"`
	Strategies []string `json:"strategies"`
	Sigkills   int      `json:"sigkills"`
	Hooks      int      `json:"hook_crashes"`
	Redis      int      `json:"redis_restarts"`
	Postgres   int      `json:"postgres_restarts"`
	HoldTTL    string   `json:"hold_ttl,omitempty"` // overrides the profile's hold time
	PayMinMs   int      `json:"payment_delay_min_ms,omitempty"`
	PayMaxMs   int      `json:"payment_delay_max_ms,omitempty"`
	Items      int      `json:"items,omitempty"` // overrides the default
	Buyers     int      `json:"buyers,omitempty"`
	WindowS    int      `json:"window_s,omitempty"`
	Control    bool     `json:"negative_control,omitempty"` // expected to violate an invariant: it proves the checker can see the problem
	Room       bool     `json:"waiting_room"`
	About      string   `json:"about"`
}

func defaultPlans() []e4Plan {
	all := []string{"locking", "optimistic", "fastpath"}
	return []e4Plan{
		{Name: "api-kills", Strategies: all, Sigkills: 2, Hooks: 3, About: "an API instance is killed at random moments (SIGKILL) and at exact named points (armed crash hooks) while buyers are mid-hold and mid-confirm"},
		{Name: "redis-restart", Strategies: []string{"fastpath"}, Redis: 2, Room: true, About: "Redis, which holds the fast-path gate and the waiting room, is restarted (it runs without persistence, so it comes back empty) twice during load"},
		{Name: "postgres-restart", Strategies: all, Postgres: 1, About: "Postgres is killed and restarted during load: in-flight transactions are lost, the API loses its connections"},
		{Name: "expiry-race", Strategies: all, HoldTTL: "400ms", PayMinMs: 150, PayMaxMs: 900, Items: 300,
			About: "holds last 400 ms while payments take 150-900 ms, so many confirms arrive after the hold expired and often after another buyer took the item"},
		// The negative control: the naive strategy on the schema without the unique index, under enough contention
		// that it double sells. It is reported apart from the real plans. Earlier trials showed it does NOT fire at
		// 100 items or at low arrival rates, so its parameters are fixed here and it is repeated like the others.
		{Name: "control", Strategies: []string{"naive-unprotected"}, HoldTTL: "400ms", PayMinMs: 150, PayMaxMs: 900,
			Items: 50, Buyers: 4000, WindowS: 5, Control: true,
			About: "negative control: no protection, high contention: the checker must see double sales, otherwise a zero elsewhere means nothing"},
	}
}

type executedEvent struct {
	chaosEvent
	ActualAt time.Duration `json:"actual_at_ns"` // when it began, from the start of the arrivals
	// Late: it began after the arrival window had ended, so it did not happen under arriving load.
	Late   bool   `json:"began_after_arrivals_ended"`
	Result string `json:"result"`
}

type e4Record struct {
	Plan      e4Plan              `json:"plan"`
	Strategy  string              `json:"strategy"`
	Run       int                 `json:"run"`
	Items     int                 `json:"items"`
	Buyers    int                 `json:"buyers"`
	ArrivalS  float64             `json:"arrival_window_s"`
	Skew      float64             `json:"skew_zipf_exponent"`
	Seed      uint64              `json:"seed"`
	Profile   string              `json:"payment_profile"`
	Env       bench.Environment   `json:"environment"`
	Stores    bench.StoreSettings `json:"store_settings"`
	StartedAt string              `json:"started_at"`
	WallS     float64             `json:"wall_s"`
	Client    loadgen.Summary     `json:"client_summary"`

	Scheduled []chaosEvent    `json:"scheduled"`
	Executed  []executedEvent `json:"executed"`
	// Kills, counted from what the supervisor actually observed: a process that really died.
	SigkillsObserved  int64 `json:"api_sigkills_observed"`
	HookExitsObserved int64 `json:"api_hook_crash_exits_observed"`
	OtherExits        int64 `json:"api_other_exits"` // exits that were neither: the API could not reach Postgres, for example
	APIRestarts       int64 `json:"api_restarts"`
	RedisRestarts     int   `json:"redis_restarts_done"`
	PostgresRestarts  int   `json:"postgres_restarts_done"`
	HooksNeverFired   int   `json:"armed_hooks_never_fired"`
	LateEvents        int   `json:"events_after_arrivals_ended"`
	TotalKills        int64 `json:"total_api_kills"`

	Server     json.RawMessage  `json:"server_metrics_last_process"` // counters restart with every process: only the last is shown
	Checker    checker.Report   `json:"checker"`
	Reconcile  reconcile.Report `json:"reconcile"`
	Violations []string         `json:"violations"`
	Verdict    string           `json:"verdict"`
}

func (s *session) e4Env(h *harness, plan e4Plan, strategy, token string, arrivalRate float64) []string {
	spec := allStrategies[2]
	for _, st := range allStrategies {
		if st.Name == strategy {
			spec = st
		}
	}
	ttl := s.ttl
	if plan.HoldTTL != "" {
		ttl = plan.HoldTTL
	}
	env := []string{"DATABASE_URL=" + h.dbURL, "REDIS_ADDR=" + h.redis, "STRATEGY=" + spec.Strategy, "SCHEMA=" + spec.Schema,
		"PAYMENT_PROFILE=" + s.profile, "HOLD_TTL=" + ttl, "DB_POOL_SIZE=32", "FAULTS_ENABLED=true", "ADMIN_TOKEN=" + token}
	if plan.PayMinMs > 0 {
		env = append(env, fmt.Sprintf("PAYMENT_DELAY_MIN_MS=%d", plan.PayMinMs), fmt.Sprintf("PAYMENT_DELAY_MAX_MS=%d", plan.PayMaxMs))
	}
	if plan.Room {
		env = append(env, fmt.Sprintf("ROOM_RATE=%g", 2*arrivalRate)) // generous: the room is there to be disrupted, not to throttle
	}
	return env
}

// arm makes the running API crash the next time any request reaches point.
func armCrash(base, token, point string) error {
	body := strings.NewReader(`{"action":"crash","remaining":1}`)
	req, _ := http.NewRequest("PUT", base+"/admin/faults/"+point, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("arming %s: status %d", point, resp.StatusCode)
	}
	return nil
}

// oneE4Run executes one run of one plan against one strategy and writes its record.
func (s *session) oneE4Run(ctx context.Context, plan e4Plan, strategy string, run int, items, buyers int, window time.Duration, skew float64, seed uint64, store storeCommands) (*e4Record, error) {
	h := s.h
	if err := h.reset(ctx); err != nil {
		return nil, err
	}
	// The waiting room keeps its own keys: reset those too.
	rdb := redis.NewClient(&redis.Options{Addr: h.redis})
	defer rdb.Close()
	_ = fastpathWipe(ctx, rdb, "rroom:")

	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)
	rate := float64(buyers) / window.Seconds()
	sup, err := newSupervisor(h, s.e4Env(h, plan, strategy, token, rate))
	if err != nil {
		return nil, err
	}
	if err := sup.start(ctx); err != nil {
		return nil, err
	}
	defer sup.stop()

	spec := allStrategies[2]
	for _, st := range allStrategies {
		if st.Name == strategy {
			spec = st
		}
	}
	pool, err := db.Open(ctx, h.dbURL, spec.Schema, db.WithMaxConns(4))
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	admin, err := db.Open(ctx, h.dbURL, "", db.WithMaxConns(2))
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	stores := bench.CaptureStores(ctx, admin, rdb)

	eventID, itemIDs, err := loadgen.CreateEvent(ctx, sup.base, items)
	if err != nil {
		return nil, err
	}
	schedule := buildSchedule(plan.Sigkills, plan.Hooks, plan.Redis, plan.Postgres, window, strategy == "fastpath", seed)

	chaosCtx, stopChaos := context.WithCancel(ctx)
	var executed []executedEvent
	var redisDone, pgDone int
	chaosFinished := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(chaosFinished)
		for _, ev := range schedule {
			select {
			case <-chaosCtx.Done():
				return
			case <-time.After(time.Until(started.Add(ev.At))):
			}
			began := time.Since(started)
			res := "done"
			switch ev.Kind {
			case kSigkill:
				sup.sigkill()
			case kHook:
				var err error
				for attempt := 0; attempt < 25; attempt++ { // the API may be down from the previous event
					if err = armCrash(sup.base, token, ev.Point); err == nil {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				if err != nil {
					res = "not armed: " + err.Error()
				}
			case kRedis:
				if err := store.run(chaosCtx, "redis"); err != nil {
					res = err.Error()
				} else {
					redisDone++
				}
			case kPostgres:
				if err := store.run(chaosCtx, "postgres"); err != nil {
					res = err.Error()
				} else {
					pgDone++
					if err := waitPostgres(chaosCtx, admin, 60*time.Second); err != nil {
						res = "restarted, but " + err.Error()
					}
				}
			}
			executed = append(executed, executedEvent{chaosEvent: ev, ActualAt: began, Late: began > window, Result: res})
		}
	}()

	runID := fmt.Sprintf("e4-%s-%s-r%d", plan.Name, strategy, run)
	res, loadErr := loadgen.Run(ctx, loadgen.Config{BaseURL: sup.base, ItemIDs: itemIDs, Picker: workload.NewPicker(len(itemIDs), skew, seed),
		Rate: rate, Duration: window, RunID: runID, Room: plan.Room})
	stopChaos()
	<-chaosFinished
	if loadErr != nil {
		return nil, loadErr
	}

	// Disarm everything, remember how many armed hooks never fired, and make sure the API is back.
	var neverFired int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM fault_config WHERE action = 'crash' AND remaining > 0`).Scan(&neverFired)
	_, _ = pool.Exec(ctx, `DELETE FROM fault_config`)
	if err := waitPostgres(ctx, pool, 60*time.Second); err != nil {
		return nil, err
	}
	if err := sup.waitUp(60 * time.Second); err != nil {
		return nil, err
	}
	server, _ := fetch(sup.base + "/metrics")

	chk, err := checker.Check(ctx, pool)
	if err != nil {
		return nil, err
	}
	rec, err := reconcile.Check(ctx, pool, eventID, res.Buyers)
	if err != nil {
		return nil, err
	}

	late := 0
	for _, ev := range executed {
		if ev.Late {
			late++
		}
	}
	r := &e4Record{Plan: plan, Strategy: strategy, Run: run, Items: items, Buyers: buyers, ArrivalS: window.Seconds(), Skew: skew, Seed: seed,
		Profile: s.profile, Env: h.env, Stores: stores, StartedAt: started.UTC().Format(time.RFC3339), WallS: time.Since(started).Seconds(),
		Client: res.Summary, Scheduled: schedule, Executed: executed, SigkillsObserved: sup.Sigkills.Load(), HookExitsObserved: sup.HookExits.Load(),
		OtherExits: sup.OtherExit.Load(), APIRestarts: sup.Restarts.Load(), RedisRestarts: redisDone, PostgresRestarts: pgDone,
		HooksNeverFired: neverFired, LateEvents: late, TotalKills: sup.Sigkills.Load() + sup.HookExits.Load(), Server: server, Checker: chk, Reconcile: rec}
	if !chk.OK() {
		r.Violations = append(r.Violations, "checker: "+chk.String())
	}
	if !rec.OK() {
		r.Violations = append(r.Violations, "reconcile: "+rec.String())
	}
	r.Verdict = "all invariants held"
	if len(r.Violations) > 0 {
		r.Verdict = "INVARIANT VIOLATED"
	}
	return r, s.writeE4(r, res.Buyers)
}

func waitPostgres(ctx context.Context, pool interface{ Ping(context.Context) error }, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := pool.Ping(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("Postgres did not answer within %v", timeout)
}

func fastpathWipe(ctx context.Context, rdb *redis.Client, prefix string) error {
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, prefix+"*", 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		if cursor = next; cursor == 0 {
			return nil
		}
	}
}

func (s *session) writeE4(r *e4Record, buyers []loadgen.BuyerResult) error {
	if err := os.MkdirAll(s.h.out, 0o755); err != nil {
		return err
	}
	base := filepath.Join(s.h.out, fmt.Sprintf("e4-%s-%s-run%d", r.Plan.Name, r.Strategy, r.Run))
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(base+".json", b, 0o644); err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for _, bu := range buyers {
		enc.Encode(bu)
	}
	zw.Close()
	return os.WriteFile(base+".buyers.ndjson.gz", buf.Bytes(), 0o644)
}

// e4 runs the failure experiments: real kills and store restarts under load, judged by the independent
// checker and the reconciler. Every run is reported.
func e4(args []string) error {
	fs := flag.NewFlagSet("e4", flag.ExitOnError)
	c := addCommon(fs, "benchmark-scratch/e4")
	plans := fs.String("plans", "api-kills,redis-restart,postgres-restart,expiry-race,control", "comma-separated plans")
	strategies := fs.String("strategies", "", "limit to these strategies (default: each plan's own)")
	runs := fs.Int("runs", 5, "runs per plan and strategy")
	items := fs.Int("items", 1000, "items in the event")
	buyers := fs.Int("buyers", 3000, "virtual buyers per run")
	arrival := fs.Duration("arrival", 20*time.Second, "window over which the buyers arrive; the chaos events fall inside it")
	storeWindow := fs.Duration("store-window", 40*time.Second, "arrival window for the plans that restart a store; buyers scale to keep the arrival rate")
	skew := fs.Float64("skew", 1.1, "Zipf exponent of demand")
	sigkills := fs.Int("sigkills", -1, "override the number of SIGKILLs per run in the api-kills plan")
	hooks := fs.Int("hook-crashes", -1, "override the number of armed crash hooks per run in the api-kills plan")
	restartRedis := fs.String("restart-redis-cmd", "", "command that restarts Redis (default: docker compose restart -t 0 redis)")
	restartPG := fs.String("restart-postgres-cmd", "", "command that restarts Postgres (default: docker compose restart -t 0 postgres)")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	s, err := c.open(ctx, "e4")
	if err != nil {
		return err
	}
	s.h.allowFaults = true // the failure experiments are the one place faults may be enabled (D13)
	store := storeCommands{redis: *restartRedis, postgres: *restartPG}

	want := map[string]bool{}
	for _, n := range strings.Split(*plans, ",") {
		want[strings.TrimSpace(n)] = true
	}
	var limit map[string]bool
	if *strategies != "" {
		limit = map[string]bool{}
		for _, n := range strings.Split(*strategies, ",") {
			limit[strings.TrimSpace(n)] = true
		}
	}
	pc, _ := payment.Profile(c.profile)
	fmt.Printf("E4: %d buyers over %v, %d items, skew %g, payment %s (%v-%v), %d runs per plan and strategy\n",
		*buyers, *arrival, *items, *skew, c.profile, pc.DelayMin, pc.DelayMax, *runs)

	type key struct{ plan, strategy string }
	var order []key
	agg := map[key][]*e4Record{}
	fmt.Printf("\n%-16s %-18s %3s | %6s %6s %6s %6s | %7s %7s | %-19s %5s %5s\n", "plan", "strategy", "run", "sigkil", "hooks", "redis", "pg", "unknown", "->booked", "invariants", "late", "unfd")
	for run := 1; run <= *runs; run++ { // interleaved by run number
		for _, plan := range defaultPlans() {
			if !want[plan.Name] {
				continue
			}
			if plan.Name == "api-kills" {
				if *sigkills >= 0 {
					plan.Sigkills = *sigkills
				}
				if *hooks >= 0 {
					plan.Hooks = *hooks
				}
			}
			planItems := *items
			if plan.Items > 0 && *items == 1000 {
				planItems = plan.Items
			}
			// With -strategies the list replaces the plan's own. That is how the negative control is run:
			// the naive strategy on the unprotected schema must be flagged by the same checker.
			strategyList := plan.Strategies
			if limit != nil && !plan.Control { // the control always runs the naive strategy
				strategyList = nil
				for _, n := range strings.Split(*strategies, ",") {
					if _, err := pickStrategies(strings.TrimSpace(n)); err != nil {
						return err
					}
					strategyList = append(strategyList, strings.TrimSpace(n))
				}
			}
			for _, strategy := range strategyList {
				window, planBuyers := *arrival, *buyers
				if plan.Buyers > 0 { // a plan with its own workload (the control)
					window, planBuyers = time.Duration(plan.WindowS)*time.Second, plan.Buyers
				}
				if (plan.Redis > 0 || plan.Postgres > 0) && *storeWindow > window {
					// A store restart takes several seconds and events run one after another: use a longer
					// window at the same arrival rate so the failures still fall inside the load.
					planBuyers = int(float64(*buyers) * storeWindow.Seconds() / arrival.Seconds())
					window = *storeWindow
				}
				r, err := s.oneE4Run(ctx, plan, strategy, run, planItems, planBuyers, window, *skew, c.seed+uint64(run)*7919, store)
				if err != nil {
					return fmt.Errorf("%s %s run %d: %w", plan.Name, strategy, run, err)
				}
				k := key{plan.Name, strategy}
				if _, seen := agg[k]; !seen {
					order = append(order, k)
				}
				agg[k] = append(agg[k], r)
				unknown := r.Client.HoldErrors + r.Client.ConfirmErrors + r.Client.RoomErrors
				fmt.Printf("%-16s %-18s %3d | %6d %6d %6d %6d | %7d %7d | %-19s %5d %5d\n", plan.Name, strategy, run, r.SigkillsObserved, r.HookExitsObserved,
					r.RedisRestarts, r.PostgresRestarts, unknown, r.Reconcile.UnknownOutcomeBooked, r.Verdict, r.LateEvents, r.HooksNeverFired)
			}
		}
	}

	var totalRuns, totalKills, violated, lateTotal, controlRuns, controlFired int
	var redisTotal, pgTotal int
	for _, k := range order {
		for _, r := range agg[k] {
			totalRuns++
			totalKills += int(r.TotalKills)
			lateTotal += r.LateEvents
			redisTotal += r.RedisRestarts
			pgTotal += r.PostgresRestarts
			switch {
			case r.Plan.Control:
				controlRuns++
				if len(r.Violations) > 0 {
					controlFired++
				}
			case len(r.Violations) > 0:
				violated++
			}
		}
	}
	fmt.Printf("\nE4 totals: %d runs, %d API kills observed (SIGKILL plus crash hooks), %d Redis restarts, %d Postgres restarts, %d runs with a violated invariant\n",
		totalRuns, totalKills, redisTotal, pgTotal, violated)
	if controlRuns > 0 {
		fmt.Printf("negative control (naive strategy, no unique index, high contention): the checker flagged a violation in %d of %d runs\n", controlFired, controlRuns)
		if controlFired == 0 {
			fmt.Println("WARNING: the control never fired, so a zero elsewhere in this session is not evidence: raise the contention (fewer items, higher rate)")
		}
	}
	if lateTotal > 0 {
		fmt.Printf("WARNING: %d chaos events began after the arrivals had ended (late): those did not happen under arriving load\n", lateTotal)
	}
	fmt.Printf("raw records in %s\n", c.out)
	return nil
}
