package config

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Setenv("HOLD_TTL", "")
	t.Setenv("DB_POOL_SIZE", "")
	c := FromEnv()
	if c.PoolSize != 32 || c.HoldTTL != 300*time.Second {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	t.Setenv("HOLD_TTL", "2s")
	t.Setenv("DB_POOL_SIZE", "8")
	c := FromEnv()
	if c.PoolSize != 8 || c.HoldTTL != 2*time.Second {
		t.Fatalf("overrides not applied: %+v", c)
	}
}
