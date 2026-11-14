package main

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// A run must start from an empty Redis, the waiting room's keys included. Buyer ids repeat across the arms of a
// surge experiment, so a pass left over from the previous arm lets a "new" buyer straight in and the room never
// throttles. (Found on 2026-11-14: 128,000 stale passes made a rehearsal's room arms admit about 80% of
// buyers at once.)
func TestResetWipesTheWaitingRoomsKeysAsWellAsTheGate(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: defaultRedis(), DialTimeout: 500 * time.Millisecond})
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable (run scripts/up first): %v", err)
	}
	t.Setenv("ROOM_PREFIX", "")
	keep := "bench-test-other:key"
	for _, k := range []string{"rre:test-gate", "rroom:bp:test-buyer", "rroom:queue", "rroom:hb:test-buyer", keep} {
		if err := rdb.Set(ctx, k, "1", time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
	}
	defer rdb.Del(context.Background(), keep)

	if err := wipeRedisState(ctx, rdb); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"rre:test-gate", "rroom:bp:test-buyer", "rroom:queue", "rroom:hb:test-buyer"} {
		if n, _ := rdb.Exists(ctx, k).Result(); n != 0 {
			t.Fatalf("%s survived the reset", k)
		}
	}
	if n, _ := rdb.Exists(ctx, keep).Result(); n != 1 {
		t.Fatal("the reset must not touch keys that belong to nobody's run")
	}
}

func TestResetFollowsAConfiguredRoomPrefix(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: defaultRedis(), DialTimeout: 500 * time.Millisecond})
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable (run scripts/up first): %v", err)
	}
	t.Setenv("ROOM_PREFIX", "benchroom-test:")
	if err := rdb.Set(ctx, "benchroom-test:bp:x", "1", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := wipeRedisState(ctx, rdb); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, "benchroom-test:bp:x").Result(); n != 0 {
		t.Fatal("a key under the configured room prefix survived the reset")
	}
}
