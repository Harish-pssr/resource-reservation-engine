package fastpath

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Sold is the key value for an item that is confirmed in the database.
const Sold = "sold"

// Claim values have the form "buyer|requestID". A buyer id must not contain '|'.
func claimValue(buyer, requestID string) string { return buyer + "|" + requestID }

// Entry is one key to write back after a database answer (negative caching, D8).
type Entry struct {
	ItemID int64
	Value  string
	TTL    time.Duration // 0 = no expiry (used for sold items)
}

// Gate is the in-memory claim layer in front of the database. It is only ever a hint:
// it may be empty, stale, or unreachable, and correctness never depends on it (D8).
type Gate interface {
	// Claim atomically claims every item for value, or none. ok=false means at least one item
	// is claimed by someone else. A claim that already holds the same value succeeds.
	Claim(ctx context.Context, ids []int64, value string, ttl time.Duration) (ok bool, err error)
	// Extend lengthens the claims, only for items whose value is exactly this one.
	Extend(ctx context.Context, ids []int64, value string, ttl time.Duration) error
	// Drop deletes the claims of this buyer on these items; claims owned by others are untouched.
	Drop(ctx context.Context, ids []int64, buyer string) error
	// MarkSold records that the items are confirmed in the database. No expiry.
	MarkSold(ctx context.Context, ids []int64) error
	// Learn writes back what the database said, without overwriting an existing key.
	Learn(ctx context.Context, entries []Entry) error
}

// RedisGate implements Gate on one Redis instance. Multi-key scripts need all keys on one
// server; a Redis Cluster would need hash tags (see docs/limitations.md).
type RedisGate struct {
	rdb    *redis.Client
	prefix string
}

// NewRedisGate returns a gate whose keys all start with prefix.
func NewRedisGate(rdb *redis.Client, prefix string) *RedisGate {
	return &RedisGate{rdb: rdb, prefix: prefix}
}

func (g *RedisGate) keys(ids []int64) []string {
	k := make([]string, len(ids))
	for i, id := range ids {
		k[i] = g.prefix + "item:" + strconv.FormatInt(id, 10)
	}
	return k
}

// Each script runs atomically inside Redis: nothing can interleave between its steps.
var (
	// All-or-none claim: refuse if any key holds a different value, else set them all.
	claimScript = redis.NewScript(`
		local value = ARGV[1]
		for i = 1, #KEYS do
			local v = redis.call('GET', KEYS[i])
			if v and v ~= value then return 0 end
		end
		for i = 1, #KEYS do redis.call('SET', KEYS[i], value, 'PX', ARGV[2]) end
		return 1`)

	// Extend only the keys that still carry exactly our value.
	extendScript = redis.NewScript(`
		for i = 1, #KEYS do
			if redis.call('GET', KEYS[i]) == ARGV[1] then redis.call('PEXPIRE', KEYS[i], ARGV[2]) end
		end
		return 1`)

	// Delete only keys whose value starts with "buyer|".
	dropScript = redis.NewScript(`
		for i = 1, #KEYS do
			local v = redis.call('GET', KEYS[i])
			if v and string.sub(v, 1, #ARGV[1]) == ARGV[1] then redis.call('DEL', KEYS[i]) end
		end
		return 1`)
)

func ms(d time.Duration) int64 {
	if m := d.Milliseconds(); m > 0 {
		return m
	}
	return 1
}

func (g *RedisGate) Claim(ctx context.Context, ids []int64, value string, ttl time.Duration) (bool, error) {
	n, err := claimScript.Run(ctx, g.rdb, g.keys(ids), value, ms(ttl)).Int()
	return n == 1, err
}

func (g *RedisGate) Extend(ctx context.Context, ids []int64, value string, ttl time.Duration) error {
	return extendScript.Run(ctx, g.rdb, g.keys(ids), value, ms(ttl)).Err()
}

func (g *RedisGate) Drop(ctx context.Context, ids []int64, buyer string) error {
	return dropScript.Run(ctx, g.rdb, g.keys(ids), buyer+"|").Err()
}

func (g *RedisGate) MarkSold(ctx context.Context, ids []int64) error {
	pipe := g.rdb.Pipeline()
	for _, k := range g.keys(ids) {
		pipe.Set(ctx, k, Sold, 0)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (g *RedisGate) Learn(ctx context.Context, entries []Entry) error {
	pipe := g.rdb.Pipeline()
	for _, e := range entries {
		pipe.SetNX(ctx, g.keys([]int64{e.ItemID})[0], e.Value, e.TTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Get returns the value of an item's key ("" if absent). Used by tests.
func (g *RedisGate) Get(ctx context.Context, id int64) (string, error) {
	v, err := g.rdb.Get(ctx, g.keys([]int64{id})[0]).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

// Wipe deletes every key of this gate: the state a restarted, non-persistent Redis would have.
// Tests use it to simulate losing the cache; the real kill-and-restart experiment is separate (E4).
func (g *RedisGate) Wipe(ctx context.Context) error {
	var cursor uint64
	for {
		keys, next, err := g.rdb.Scan(ctx, cursor, g.prefix+"*", 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := g.rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		if cursor = next; cursor == 0 {
			return nil
		}
	}
}
