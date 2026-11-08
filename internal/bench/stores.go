package bench

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// StoreSettings records how the two stores were configured when a result was produced. A latency or
// capacity figure is only meaningful together with these.
type StoreSettings struct {
	PostgresVersion  string            `json:"postgres_version"`
	PostgresSettings map[string]string `json:"postgres_settings"`
	RedisVersion     string            `json:"redis_version,omitempty"`
	RedisPersistence string            `json:"redis_persistence,omitempty"` // "none" is the intended test configuration (D8)
}

// postgresSettingNames are the settings that most change write and lock behaviour.
var postgresSettingNames = []string{
	"max_connections", "shared_buffers", "work_mem", "effective_cache_size", "synchronous_commit", "fsync",
	"full_page_writes", "wal_level", "max_wal_size", "checkpoint_timeout", "deadlock_timeout", "default_transaction_isolation",
}

// CaptureStores reads the settings. A store it cannot reach is left blank, never guessed.
func CaptureStores(ctx context.Context, pg *pgxpool.Pool, rdb *redis.Client) StoreSettings {
	out := StoreSettings{PostgresSettings: map[string]string{}}
	if pg != nil {
		_ = pg.QueryRow(ctx, `SELECT version()`).Scan(&out.PostgresVersion)
		if rows, err := pg.Query(ctx, `SELECT name, setting || COALESCE(unit, '') FROM pg_settings WHERE name = ANY($1)`, postgresSettingNames); err == nil {
			defer rows.Close()
			for rows.Next() {
				var name, val string
				if rows.Scan(&name, &val) == nil {
					out.PostgresSettings[name] = val
				}
			}
		}
	}
	if rdb != nil {
		if info, err := rdb.Info(ctx, "server", "persistence").Result(); err == nil {
			out.RedisVersion = infoValue(info, "redis_version")
			switch {
			case infoValue(info, "aof_enabled") == "1":
				out.RedisPersistence = "aof"
			case infoValue(info, "rdb_last_save_time") != "" && infoValue(info, "rdb_changes_since_last_save") != "":
				out.RedisPersistence = "rdb snapshots possible: check the save setting"
			}
		}
		if cfg, err := rdb.ConfigGet(ctx, "save").Result(); err == nil && cfg["save"] == "" && out.RedisPersistence != "aof" {
			out.RedisPersistence = "none"
		}
	}
	return out
}

func infoValue(info, key string) string {
	for _, line := range strings.Split(strings.ReplaceAll(info, "\r", ""), "\n") {
		if rest, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
