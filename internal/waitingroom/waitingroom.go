// Package waitingroom is the admission control in front of the reserve path (D9). Buyers join a
// FIFO queue held in Redis, and are admitted at a global rate R: each admitted buyer receives a
// pass (a random token with a time limit) that POST /holds requires.
//
// The room only limits how fast buyers reach the store. It never decides who gets an item: that is
// still the database's job (D2), so losing the room's state can lose order and positions but can
// never cause a double sale.
package waitingroom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// State is what a buyer is told.
type State string

const (
	Admitted  State = "admitted"   // the buyer has a pass
	Queued    State = "queued"     // waiting; Position says where
	Retry     State = "retry"      // the room is full: try again later
	NotQueued State = "not_queued" // unknown buyer (never joined, or the queue was lost): join again
)

// Config tunes the room. Rate is the admission rate R in buyers per second, global across instances.
type Config struct {
	Rate         float64
	Burst        int           // most buyers admitted in one instant; default Rate/5
	MaxQueue     int           // beyond this a join gets Retry
	PassTTL      time.Duration // how long an admission pass lasts
	HeartbeatTTL time.Duration // a queued buyer must poll at least this often or the ticket is skipped
	Tick         time.Duration // how often Run asks Redis to admit; default 50 ms
}

func (c *Config) defaults() {
	if c.Burst <= 0 {
		c.Burst = max(1, int(c.Rate/5))
	}
	if c.MaxQueue <= 0 {
		c.MaxQueue = 100000
	}
	if c.PassTTL <= 0 {
		c.PassTTL = 5 * time.Minute
	}
	if c.HeartbeatTTL <= 0 {
		c.HeartbeatTTL = 30 * time.Second
	}
	if c.Tick <= 0 {
		c.Tick = 50 * time.Millisecond
	}
}

// Status is the answer to a join or a poll.
type Status struct {
	State      State         `json:"state"`
	Position   int           `json:"position,omitempty"`    // 1 = next to be admitted
	Ticket     int64         `json:"ticket,omitempty"`      // arrival sequence number, strictly increasing
	Token      string        `json:"token,omitempty"`       // the pass, when admitted
	AdmitOrder int64         `json:"admit_order,omitempty"` // 1,2,3... in the order buyers were admitted
	RetryAfter time.Duration `json:"-"`
}

// Stats counts what the room did; they appear at /metrics.
type Stats struct {
	Joins, Queued, Retries, AlreadyAdmitted atomic.Int64
	Polls, NotQueued                        atomic.Int64
	Admitted, StaleSkipped                  atomic.Int64
	PassChecks, PassRejected                atomic.Int64
}

// Room is the waiting room.
type Room struct {
	rdb    *redis.Client
	prefix string
	cfg    Config
	Stats  Stats
}

// New returns a Room whose Redis keys all start with prefix.
func New(rdb *redis.Client, prefix string, cfg Config) *Room {
	cfg.defaults()
	return &Room{rdb: rdb, prefix: prefix, cfg: cfg}
}

// Config returns the effective configuration (recorded with benchmark results).
func (r *Room) Config() Config { return r.cfg }

func (r *Room) k(parts ...string) string {
	s := r.prefix
	for _, p := range parts {
		s += p
	}
	return s
}

// All scripts run atomically inside Redis. They read and write per-buyer keys that are built from the
// prefix inside the script, so they need a single Redis instance (a Cluster would need hash tags).
var (
	// Join: a buyer who already has a pass gets it back; one already queued keeps their place;
	// a full queue says retry; otherwise they take the next sequence number.
	joinScript = redis.NewScript(`
		local prefix, buyer = ARGV[1], ARGV[2]
		local tok = redis.call('GET', prefix..'bp:'..buyer)
		if tok then
			return {'admitted', tok, tonumber(redis.call('GET', prefix..'ao:'..buyer) or 0), 0, 0}
		end
		local queue = prefix..'queue'
		local rank = redis.call('ZRANK', queue, buyer)
		if rank then
			redis.call('SET', prefix..'hb:'..buyer, '1', 'PX', ARGV[4])
			return {'queued', rank + 1, 0, tonumber(redis.call('ZSCORE', queue, buyer)), 0}
		end
		if redis.call('ZCARD', queue) >= tonumber(ARGV[3]) then return {'retry', 0, 0, 0, 0} end
		local ticket = redis.call('INCR', prefix..'seq')
		redis.call('ZADD', queue, ticket, buyer)
		redis.call('SET', prefix..'hb:'..buyer, '1', 'PX', ARGV[4])
		return {'queued', redis.call('ZRANK', queue, buyer) + 1, 0, ticket, 0}`)

	// Poll: like join but never enqueues. Polling is what keeps a ticket alive.
	pollScript = redis.NewScript(`
		local prefix, buyer = ARGV[1], ARGV[2]
		local tok = redis.call('GET', prefix..'bp:'..buyer)
		if tok then
			return {'admitted', tok, tonumber(redis.call('GET', prefix..'ao:'..buyer) or 0), 0, 0}
		end
		local queue = prefix..'queue'
		local rank = redis.call('ZRANK', queue, buyer)
		if rank then
			redis.call('SET', prefix..'hb:'..buyer, '1', 'PX', ARGV[3])
			return {'queued', rank + 1, 0, tonumber(redis.call('ZSCORE', queue, buyer)), 0}
		end
		return {'not_queued', 0, 0, 0, 0}`)

	// Admit: refill a token bucket from Redis's own clock, then admit queued buyers from the head
	// while tokens last. A head ticket whose heartbeat expired is dropped without spending a token.
	// ARGV: prefix, rate, burst, maxScan, passTTLms, then pre-generated random tokens.
	admitScript = redis.NewScript(`
		local prefix = ARGV[1]
		local rate, burst, maxScan, ttl = tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]), ARGV[5]
		local t = redis.call('TIME')
		local now = t[1] * 1000 + math.floor(t[2] / 1000)
		local budget = prefix..'budget'
		local b = redis.call('HMGET', budget, 'tokens', 'ts')
		local tokens, ts = tonumber(b[1]), tonumber(b[2])
		if not tokens then tokens, ts = burst, now end
		tokens = math.min(burst, tokens + (now - ts) * rate / 1000)
		local admitted, skipped, scanned, ti = 0, 0, 0, 6
		while tokens >= 1 and scanned < maxScan and ARGV[ti] do
			local head = redis.call('ZPOPMIN', prefix..'queue')
			if #head == 0 then break end
			local buyer = head[1]
			scanned = scanned + 1
			if redis.call('EXISTS', prefix..'hb:'..buyer) == 1 then
				local order = redis.call('INCR', prefix..'admitted')
				redis.call('SET', prefix..'pass:'..ARGV[ti], buyer, 'PX', ttl)
				redis.call('SET', prefix..'bp:'..buyer, ARGV[ti], 'PX', ttl)
				redis.call('SET', prefix..'ao:'..buyer, order, 'PX', ttl)
				redis.call('DEL', prefix..'hb:'..buyer)
				ti = ti + 1
				tokens = tokens - 1
				admitted = admitted + 1
			else
				skipped = skipped + 1
			end
		end
		redis.call('HSET', budget, 'tokens', tostring(tokens), 'ts', tostring(now))
		return {admitted, skipped}`)
)

func parse(v any) (Status, error) {
	a, ok := v.([]any)
	if !ok || len(a) < 5 {
		return Status{}, fmt.Errorf("unexpected script reply %v", v)
	}
	state, _ := a[0].(string)
	st := Status{State: State(state)}
	num := func(x any) int64 { n, _ := x.(int64); return n }
	switch st.State {
	case Admitted:
		st.Token, _ = a[1].(string)
		st.AdmitOrder = num(a[2])
	case Queued:
		st.Position, st.Ticket = int(num(a[1])), num(a[3])
	}
	return st, nil
}

// retryAfter estimates when a queued buyer should poll again: about when their turn comes, never
// sooner than 200 ms or later than 5 s.
func (r *Room) retryAfter(position int) time.Duration {
	d := time.Duration(float64(position) / r.cfg.Rate * float64(time.Second))
	return min(max(d, 200*time.Millisecond), 5*time.Second)
}

// Join puts a buyer in the queue, or returns what they already have.
func (r *Room) Join(ctx context.Context, buyer string) (Status, error) {
	r.Stats.Joins.Add(1)
	v, err := joinScript.Run(ctx, r.rdb, nil, r.prefix, buyer, r.cfg.MaxQueue, r.cfg.HeartbeatTTL.Milliseconds()).Result()
	if err != nil {
		return Status{}, err
	}
	st, err := parse(v)
	if err != nil {
		return Status{}, err
	}
	switch st.State {
	case Queued:
		r.Stats.Queued.Add(1)
		st.RetryAfter = r.retryAfter(st.Position)
	case Retry:
		r.Stats.Retries.Add(1)
		st.RetryAfter = time.Second
	case Admitted:
		r.Stats.AlreadyAdmitted.Add(1)
	}
	return st, nil
}

// Poll reports a buyer's state and keeps their ticket alive.
func (r *Room) Poll(ctx context.Context, buyer string) (Status, error) {
	r.Stats.Polls.Add(1)
	v, err := pollScript.Run(ctx, r.rdb, nil, r.prefix, buyer, r.cfg.HeartbeatTTL.Milliseconds()).Result()
	if err != nil {
		return Status{}, err
	}
	st, err := parse(v)
	if err != nil {
		return Status{}, err
	}
	switch st.State {
	case Queued:
		st.RetryAfter = r.retryAfter(st.Position)
	case NotQueued:
		r.Stats.NotQueued.Add(1)
	}
	return st, nil
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Admit asks Redis to admit as many queued buyers as the global budget allows right now. Any number
// of API instances may call it: the budget lives in Redis, so the rate does not multiply.
func (r *Room) Admit(ctx context.Context) (admitted int, err error) {
	// Enough tokens for one tick's worth of admissions, plus slack for a burst.
	n := min(r.cfg.Burst, int(math.Ceil(r.cfg.Rate*r.cfg.Tick.Seconds()*2))+1)
	args := []any{r.prefix, r.cfg.Rate, r.cfg.Burst, 10*n + 100, r.cfg.PassTTL.Milliseconds()}
	for i := 0; i < n; i++ {
		args = append(args, newToken())
	}
	v, err := admitScript.Run(ctx, r.rdb, nil, args...).Result()
	if err != nil {
		return 0, err
	}
	a, ok := v.([]any)
	if !ok || len(a) != 2 {
		return 0, fmt.Errorf("unexpected admit reply %v", v)
	}
	adm, _ := a[0].(int64)
	skip, _ := a[1].(int64)
	r.Stats.Admitted.Add(adm)
	r.Stats.StaleSkipped.Add(skip)
	return int(adm), nil
}

// Run admits on every tick until ctx ends. Errors (Redis down) are counted by the caller's metrics
// through the next successful tick; the loop keeps going.
func (r *Room) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Admit(ctx)
		}
	}
}

// ErrNoPass is returned by Validate when the token is unknown or expired.
var ErrNoPass = errors.New("no valid admission pass")

// Validate returns the buyer a pass belongs to. A Redis error is returned as is, so the caller
// can fail open (D9).
func (r *Room) Validate(ctx context.Context, token string) (string, error) {
	r.Stats.PassChecks.Add(1)
	if token == "" {
		r.Stats.PassRejected.Add(1)
		return "", ErrNoPass
	}
	buyer, err := r.rdb.Get(ctx, r.k("pass:", token)).Result()
	if err == redis.Nil {
		r.Stats.PassRejected.Add(1)
		return "", ErrNoPass
	}
	return buyer, err
}

// QueueLength returns how many buyers are waiting.
func (r *Room) QueueLength(ctx context.Context) (int64, error) {
	return r.rdb.ZCard(ctx, r.k("queue")).Result()
}

// Counters exposes the room's counters for /metrics.
func (r *Room) Counters() map[string]int64 {
	s := &r.Stats
	return map[string]int64{
		"room_joins": s.Joins.Load(), "room_queued": s.Queued.Load(), "room_retry_full": s.Retries.Load(),
		"room_already_admitted": s.AlreadyAdmitted.Load(), "room_polls": s.Polls.Load(), "room_not_queued": s.NotQueued.Load(),
		"room_admitted": s.Admitted.Load(), "room_stale_skipped": s.StaleSkipped.Load(),
		"room_pass_checks": s.PassChecks.Load(), "room_pass_rejected": s.PassRejected.Load(),
	}
}

// Wipe deletes every key of this room (tests; the state a restarted Redis would have).
func (r *Room) Wipe(ctx context.Context) error {
	var cursor uint64
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, r.prefix+"*", 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := r.rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		if cursor = next; cursor == 0 {
			return nil
		}
	}
}
