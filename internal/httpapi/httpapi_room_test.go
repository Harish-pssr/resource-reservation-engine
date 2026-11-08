package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
	"resourcereservation/internal/httpapi"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/testdb"
	"resourcereservation/internal/waitingroom"
)

type roomFixture struct {
	t     *testing.T
	srv   *httptest.Server
	h     *httpapi.Handler
	items []int64
	room  *waitingroom.Room
}

// newRoomFixture builds the real handler with a waiting room on real Redis. addr overrides the
// Redis address (to point the room at nothing and test fail-open).
func newRoomFixture(t *testing.T, cfg waitingroom.Config, addr string) *roomFixture {
	t.Helper()
	if addr == "" {
		addr = config.FromEnv().RedisAddr
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 300 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { rdb.Close() })
	if addr == config.FromEnv().RedisAddr {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			t.Skipf("redis not reachable: %v", err)
		}
	}
	room := waitingroom.New(rdb, fmt.Sprintf("h%08x:", rand.Uint32()), cfg)
	t.Cleanup(func() { room.Wipe(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go room.Run(ctx)

	pool := testdb.New(t, true)
	pcfg := payment.Config{DelayMin: time.Millisecond, DelayMax: time.Millisecond, Timeout: 30 * time.Millisecond}
	h := httpapi.New(httpapi.Deps{Service: locking.New(pool), Payment: payment.New(pcfg).WithRand(func() float64 { return 0.5 }),
		Pool: pool, HoldTTL: time.Minute, Room: room})
	_, items, err := db.CreateEvent(context.Background(), pool, "t", 6)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &roomFixture{t: t, srv: srv, h: h, items: items, room: room}
}

func (f *roomFixture) do(method, path string, body any, token string) (int, map[string]any) {
	f.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Admission-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *roomFixture) holdWith(token, buyer string, item int64) (int, map[string]any) {
	return f.do("POST", "/holds", map[string]any{"buyer_id": buyer, "request_id": buyer + "-r", "item_ids": []int64{item}}, token)
}

// admitBuyer joins and polls until the buyer holds a pass.
func (f *roomFixture) admitBuyer(buyer string) string {
	f.t.Helper()
	f.do("POST", "/queue/join", map[string]any{"buyer_id": buyer}, "")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, out := f.do("GET", "/queue/"+buyer, nil, "")
		if out["state"] == "admitted" {
			return out["token"].(string)
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("buyer %s was never admitted", buyer)
	return ""
}

func TestHoldNeedsAnAdmissionPass(t *testing.T) {
	f := newRoomFixture(t, waitingroom.Config{Rate: 1000}, "")
	if code, out := f.holdWith("", "alice", f.items[0]); code != http.StatusForbidden || out["outcome"] != "not_admitted" {
		t.Fatalf("no pass: %d %v", code, out)
	}
	if code, _ := f.holdWith("forged-token", "alice", f.items[0]); code != http.StatusForbidden {
		t.Fatalf("a forged pass must be refused: %d", code)
	}
	alice := f.admitBuyer("alice")
	if code, out := f.holdWith(alice, "alice", f.items[0]); code != http.StatusCreated {
		t.Fatalf("alice with her pass: %d %v", code, out)
	}
	// A pass belongs to one buyer: bob cannot use alice's.
	if code, out := f.holdWith(alice, "bob", f.items[1]); code != http.StatusForbidden || out["outcome"] != "not_admitted" {
		t.Fatalf("another buyer's pass must be refused: %d %v", code, out)
	}
}

func TestQueuedBuyerIsToldPositionAndWhenToRetry(t *testing.T) {
	f := newRoomFixture(t, waitingroom.Config{Rate: 1, Burst: 1, Tick: 20 * time.Millisecond}, "")
	f.do("POST", "/queue/join", map[string]any{"buyer_id": "first"}, "")
	time.Sleep(150 * time.Millisecond) // the first buyer takes the single burst token
	f.do("POST", "/queue/join", map[string]any{"buyer_id": "second"}, "")
	code, out := f.do("POST", "/queue/join", map[string]any{"buyer_id": "third"}, "")
	if code != http.StatusOK || out["state"] != "queued" || out["position"] != float64(2) {
		t.Fatalf("third in line: %d %v", code, out)
	}
	if out["retry_after_ms"].(float64) < 200 {
		t.Fatalf("retry_after_ms %v", out["retry_after_ms"])
	}
}

func TestFullRoomAnswersRetryAndUnknownPollIs404(t *testing.T) {
	f := newRoomFixture(t, waitingroom.Config{Rate: 0.001, Burst: 1, MaxQueue: 2, Tick: time.Hour}, "")
	f.do("POST", "/queue/join", map[string]any{"buyer_id": "a"}, "")
	f.do("POST", "/queue/join", map[string]any{"buyer_id": "b"}, "")
	if code, out := f.do("POST", "/queue/join", map[string]any{"buyer_id": "c"}, ""); code != http.StatusTooManyRequests || out["state"] != "retry" {
		t.Fatalf("full room: %d %v", code, out)
	}
	if code, out := f.do("GET", "/queue/nobody", nil, ""); code != http.StatusNotFound || out["state"] != "not_queued" {
		t.Fatalf("unknown buyer: %d %v", code, out)
	}
}

// Redis down: the room fails open (D9). Buyers get an "open" pass and holds go through to the
// database, which stays correct. With Redis healthy the same "open" pass is refused.
func TestRoomFailsOpenWhenRedisIsDown(t *testing.T) {
	f := newRoomFixture(t, waitingroom.Config{Rate: 1000}, "127.0.0.1:1")
	code, out := f.do("POST", "/queue/join", map[string]any{"buyer_id": "alice"}, "")
	if code != http.StatusOK || out["state"] != "admitted" || out["degraded"] != true {
		t.Fatalf("join with Redis down: %d %v", code, out)
	}
	if code, out := f.holdWith(out["token"].(string), "alice", f.items[0]); code != http.StatusCreated {
		t.Fatalf("a hold must go through while Redis is down: %d %v", code, out)
	}
	if f.h.Stats.RoomFailOpen.Load() < 2 {
		t.Fatalf("failed-open requests must be counted, got %d", f.h.Stats.RoomFailOpen.Load())
	}

	healthy := newRoomFixture(t, waitingroom.Config{Rate: 1000}, "")
	if code, _ := healthy.holdWith("open", "alice", healthy.items[0]); code != http.StatusForbidden {
		t.Fatalf("the open pass must not work while Redis is healthy: %d", code)
	}
}

func TestMetricsIncludeTheRoom(t *testing.T) {
	f := newRoomFixture(t, waitingroom.Config{Rate: 1000}, "")
	f.admitBuyer("alice")
	_, m := f.do("GET", "/metrics", nil, "")
	strategy := m["strategy"].(map[string]any)
	if strategy["room_joins"].(float64) < 1 || strategy["room_admitted"].(float64) < 1 {
		t.Fatalf("room counters missing: %v", strategy)
	}
	if _, ok := strategy["room_queue_length"]; !ok {
		t.Fatalf("the queue depth must be reported: %v", strategy)
	}
}
