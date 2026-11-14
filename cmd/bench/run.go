package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"resourcereservation/internal/bench"
	"resourcereservation/internal/checker"
	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
	"resourcereservation/internal/loadgen"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reconcile"
	"resourcereservation/internal/reservation/fastpath"
	"resourcereservation/internal/workload"
)

// strategySpec names a configuration of the API under test.
type strategySpec struct {
	Name     string
	Strategy string // the API's STRATEGY
	Schema   string // protected or unprotected
}

var allStrategies = []strategySpec{
	{"naive-unprotected", "naive", db.SchemaUnprotected}, // the negative control
	{"naive-protected", "naive", db.SchemaProtected},     // the same code, the database backstop on
	{"locking", "locking", db.SchemaProtected},
	{"optimistic", "optimistic", db.SchemaProtected},
	{"fastpath", "fastpath", db.SchemaProtected},
}

// runParams are recorded in every result so a number is never separated from its conditions.
type runParams struct {
	Experiment     string  `json:"experiment"`
	Strategy       string  `json:"strategy"`
	APIStrategy    string  `json:"api_strategy"`
	Schema         string  `json:"schema"`
	Run            int     `json:"run"`
	Items          int     `json:"items"`
	Buyers         int     `json:"buyers"`
	ArrivalWindowS float64 `json:"arrival_window_s"`
	Skew           float64 `json:"skew_zipf_exponent"`
	Seed           uint64  `json:"seed"`
	PaymentProfile string  `json:"payment_profile"`
	Payment        struct {
		DelayMinMs int64   `json:"delay_min_ms"`
		DelayMaxMs int64   `json:"delay_max_ms"`
		FailPct    float64 `json:"fail_pct"`
		TimeoutPct float64 `json:"timeout_pct"`
		TimeoutMs  int64   `json:"timeout_ms"`
	} `json:"payment"`
	HoldTTL     string `json:"hold_ttl"`
	DBPoolSize  int    `json:"db_pool_size"`
	MaxInflight int    `json:"generator_max_inflight"` // the load generator's safety cap on buyers in flight; arrivals beyond it are skipped
	RetryPolicy string `json:"client_retry_policy"`
	Scope       string `json:"scope"`

	// Capacity and surge steps (D12, D9).
	Phase    string  `json:"phase,omitempty"`
	Label    string  `json:"label,omitempty"`
	RoomRate float64 `json:"room_rate_buyers_per_s,omitempty"` // 0 = no waiting room
}

// record is the raw JSON written for one run.
type record struct {
	Params    runParams           `json:"params"`
	Env       bench.Environment   `json:"environment"`
	Stores    bench.StoreSettings `json:"store_settings"`
	StartedAt string              `json:"started_at"`
	WallS     float64             `json:"wall_s"`
	Client    loadgen.Summary     `json:"client_summary"`
	Server    json.RawMessage     `json:"server_metrics"`
	StoreLoad bench.LoadSummary   `json:"store_load"`
	Samples   []bench.Sample      `json:"store_load_samples"`
	Checker   checker.Report      `json:"checker"`
	Reconcile reconcile.Report    `json:"reconcile"`
	Sustain   bench.Verdict       `json:"sustain"` // the D12 rules applied to this run
	Verdict   string              `json:"verdict"`
	// PartCPU says how much CPU each part (database, API, load generator, cache) really used, so a ceiling can be
	// attributed to the part that ran out. Linux only; a part that cannot be read is absent.
	PartCPU map[string]bench.PartCPUSummary `json:"part_cpu,omitempty"`
}

const scopeNote = "simulated payment and simulated demand; API, database and load generator share one machine unless the environment says otherwise"

// harness is what every run of a session shares.
type harness struct {
	apiBin   string
	dbURL    string
	redis    string
	out      string
	env      bench.Environment
	keepLogs bool
	// allowFaults is true only for the failure experiments (E4). Throughput, capacity and surge runs must
	// never run with fault injection enabled (D13): the hooks add polling load and could skew the numbers.
	allowFaults bool

	partsOnce sync.Once
	cgroups   map[string]string  // docker compose service -> cgroup directory
	pinned    map[string]float64 // part -> vCPUs it may use, from the core plan
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// reset puts the database, the gate and the waiting room in a known empty state before a run.
func (h *harness) reset(ctx context.Context) error {
	admin, err := db.Open(ctx, h.dbURL, "", db.WithMaxConns(2))
	if err != nil {
		return err
	}
	defer admin.Close()
	for _, s := range []string{db.SchemaProtected, db.SchemaUnprotected} {
		if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+s+" CASCADE"); err != nil {
			return fmt.Errorf("reset schema %s: %w", s, err)
		}
	}
	rdb := redis.NewClient(&redis.Options{Addr: h.redis})
	defer rdb.Close()
	return wipeRedisState(ctx, rdb)
}

// roomPrefix is the waiting room's key prefix: the API's ROOM_PREFIX, or rroom: when it is not set.
func roomPrefix() string {
	if p := strings.TrimSpace(os.Getenv("ROOM_PREFIX")); p != "" {
		return p
	}
	return "rroom:"
}

// wipeRedisState empties everything a run can leave in Redis: the fast-path gate AND the waiting room's keys
// (passes, tickets, heartbeats). Buyer ids repeat across the arms of a surge experiment, so a pass left over from
// the previous arm would let a new buyer straight in and the room would not throttle.
func wipeRedisState(ctx context.Context, rdb *redis.Client) error {
	if err := fastpath.NewRedisGate(rdb, "rre:").Wipe(ctx); err != nil {
		return err
	}
	return fastpathWipe(ctx, rdb, roomPrefix())
}

// startAPI launches the API binary as a separate process and waits until it answers.
func (h *harness) startAPI(ctx context.Context, p runParams, pc payment.Config) (*exec.Cmd, string, error) {
	port, err := freePort()
	if err != nil {
		return nil, "", err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.CommandContext(ctx, h.apiBin)
	cmd.Env = append(os.Environ(),
		"STRATEGY="+p.APIStrategy, "SCHEMA="+p.Schema, "PAYMENT_PROFILE="+p.PaymentProfile,
		"HOLD_TTL="+p.HoldTTL, fmt.Sprintf("FAULTS_ENABLED=%t", h.allowFaults), fmt.Sprintf("ROOM_RATE=%g", p.RoomRate), "HTTP_ADDR="+addr, "DATABASE_URL="+h.dbURL, "REDIS_ADDR="+h.redis,
		fmt.Sprintf("DB_POOL_SIZE=%d", p.DBPoolSize))
	if h.keepLogs {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("start api: %w", err)
	}
	base := "http://" + addr
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := fetch(base + "/metrics"); err == nil {
			if err := refuseIfFaultsEnabled(body, h.allowFaults); err != nil {
				cmd.Process.Kill()
				return nil, "", err
			}
			return cmd, base, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Kill()
	return nil, "", fmt.Errorf("the API did not become ready within 60 s")
}

func fetch(url string) (json.RawMessage, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// poolSize is the API's database pool: 32 unless the benchmark profile says otherwise (BENCH_DB_POOL_SIZE). It is
// recorded in every result.
func poolSize() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BENCH_DB_POOL_SIZE"))); err == nil && n > 0 {
		return n
	}
	return 32
}

// maxInflight is the generator's cap on buyers in flight: 20,000 unless BENCH_MAX_INFLIGHT says otherwise. A
// surge with a waiting room keeps every waiting buyer in flight, so that experiment raises it. It is
// recorded in every result.
func maxInflight() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BENCH_MAX_INFLIGHT"))); err == nil && n > 0 {
		return n
	}
	return 20000
}

// parts lists what to sample for a run: the API process, this process (the load generator), and the two stores.
func (h *harness) parts(ctx context.Context, apiPID int) []bench.Part {
	h.partsOnce.Do(func() {
		h.pinned = bench.ParsePinning(os.Getenv("BENCH_PINNING"))
		h.cgroups = map[string]string{}
		for _, svc := range []string{"postgres", "redis"} {
			if dir := bench.ContainerCgroupDir(ctx, svc); dir != "" {
				h.cgroups[svc] = dir
			}
		}
	})
	ps := []bench.Part{
		{Name: "api", VCPUs: h.pinned["api"], Read: bench.ProcessCPU(func() int { return apiPID })},
		{Name: "bench", VCPUs: h.pinned["bench"], Read: bench.SelfCPU()},
	}
	for _, svc := range []string{"postgres", "redis"} {
		if dir, ok := h.cgroups[svc]; ok {
			ps = append(ps, bench.Part{Name: svc, VCPUs: h.pinned[svc], Read: bench.CgroupCPU(dir)})
		}
	}
	return ps
}

// oneRun executes a single run of a strategy and writes its record.
func (h *harness) oneRun(ctx context.Context, p runParams, pc payment.Config) (*record, error) {
	p.MaxInflight = maxInflight()
	if err := h.reset(ctx); err != nil {
		return nil, err
	}
	cmd, base, err := h.startAPI(ctx, p, pc)
	if err != nil {
		return nil, err
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	admin, err := db.Open(ctx, h.dbURL, "", db.WithMaxConns(2))
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	sampler := bench.NewSampler(admin, time.Second)
	rdb := redis.NewClient(&redis.Options{Addr: h.redis})
	defer rdb.Close()
	stores := bench.CaptureStores(ctx, admin, rdb)

	eventID, itemIDs, err := loadgen.CreateEvent(ctx, base, p.Items)
	if err != nil {
		return nil, err
	}
	runID := fmt.Sprintf("%s-r%d", p.Strategy, p.Run)
	started := time.Now()
	sampler.Start(ctx)
	partSampler := bench.NewPartSampler(h.parts(ctx, cmd.Process.Pid), time.Second)
	partSampler.Start(ctx)
	res, err := loadgen.Run(ctx, loadgen.Config{
		BaseURL: base, ItemIDs: itemIDs, Picker: workload.NewPicker(len(itemIDs), p.Skew, p.Seed),
		Rate: float64(p.Buyers) / p.ArrivalWindowS, Duration: time.Duration(p.ArrivalWindowS * float64(time.Second)), RunID: runID,
		Room: p.RoomRate > 0, MaxInflight: p.MaxInflight})
	samples := sampler.Stop()
	partCPU := partSampler.Stop()
	if err != nil {
		return nil, err
	}
	server, err := fetch(base + "/metrics")
	if err != nil {
		return nil, err
	}

	// Judge the final state from the database alone.
	pool, err := db.Open(ctx, h.dbURL, p.Schema, db.WithMaxConns(4))
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	chk, err := checker.Check(ctx, pool)
	if err != nil {
		return nil, err
	}
	rec, err := reconcile.Check(ctx, pool, eventID, res.Buyers)
	if err != nil {
		return nil, err
	}
	r := &record{Params: p, Env: h.env, Stores: stores, StartedAt: started.UTC().Format(time.RFC3339), WallS: time.Since(started).Seconds(),
		Client: res.Summary, Server: server, StoreLoad: bench.Summarize(samples), Samples: samples, Checker: chk, Reconcile: rec, Sustain: bench.Judge(res.Summary), PartCPU: partCPU}
	switch {
	case !res.Summary.GeneratorOK:
		r.Verdict = "generator did not keep up: do not use this run for latency"
	case !chk.OK() || !rec.OK():
		r.Verdict = "invariant violated"
	default:
		r.Verdict = "all invariants held"
	}
	return r, h.write(r, res.Buyers)
}

// write saves the record, and the per-buyer log compressed next to it.
func (h *harness) write(r *record, buyers []loadgen.BuyerResult) error {
	if err := os.MkdirAll(h.out, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s-run%d", r.Params.Experiment, r.Params.Strategy, r.Params.Run)
	if r.Params.Label != "" {
		name = fmt.Sprintf("%s-%s", r.Params.Experiment, r.Params.Label)
	}
	base := filepath.Join(h.out, name)
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(base+".json", b, 0o644); err != nil {
		return err
	}
	f, err := os.Create(base + ".buyers.ndjson.gz")
	if err != nil {
		return err
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	enc := json.NewEncoder(zw)
	for _, bu := range buyers {
		if err := enc.Encode(bu); err != nil {
			return err
		}
	}
	return zw.Close()
}

func defaultDB() string    { return config.FromEnv().DatabaseURL }
func defaultRedis() string { return config.FromEnv().RedisAddr }

// refuseIfFaultsEnabled reads /metrics and refuses a measurement run against a server that has fault
// injection switched on. The failure experiments pass allow=true.
func refuseIfFaultsEnabled(metrics []byte, allow bool) error {
	var m struct {
		FaultsEnabled bool `json:"faults_enabled"`
	}
	if err := json.Unmarshal(metrics, &m); err != nil {
		return fmt.Errorf("cannot tell whether fault injection is enabled: %w", err)
	}
	if m.FaultsEnabled && !allow {
		return fmt.Errorf("fault injection is enabled on the server under test: refusing to take a measurement (D13)")
	}
	return nil
}
