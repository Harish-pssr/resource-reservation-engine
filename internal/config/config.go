// Package config reads settings from environment variables with local-development defaults.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds process settings shared by the API, load generator and checker.
type Config struct {
	DatabaseURL string
	RedisAddr   string
	HTTPAddr    string
	PoolSize    int
	HoldTTL     time.Duration

	// Waiting room (D9). RoomRate is buyers admitted per second; 0 disables the room.
	RoomRate         float64
	RoomMaxQueue     int
	RoomPassTTL      time.Duration
	RoomHeartbeatTTL time.Duration

	// Fault injection (D13). Off by default; the controls also need the admin token.
	FaultsEnabled bool
	AdminToken    string
}

// FromEnv builds a Config from the environment.
func FromEnv() Config {
	return Config{
		DatabaseURL: get("DATABASE_URL", "postgres://reservation:reservation@localhost:55432/reservation?sslmode=disable"),
		RedisAddr:   get("REDIS_ADDR", "localhost:56379"),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		PoolSize:    getInt("DB_POOL_SIZE", 32),
		HoldTTL:     getDuration("HOLD_TTL", 300*time.Second),

		RoomRate:         getFloat("ROOM_RATE", 0),
		RoomMaxQueue:     getInt("ROOM_MAX_QUEUE", 100000),
		RoomPassTTL:      getDuration("ROOM_PASS_TTL", 5*time.Minute),
		RoomHeartbeatTTL: getDuration("ROOM_HEARTBEAT_TTL", 30*time.Second),

		FaultsEnabled: os.Getenv("FAULTS_ENABLED") == "true",
		AdminToken:    os.Getenv("ADMIN_TOKEN"),
	}
}

func get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func getFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}
