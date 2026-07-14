// Package config loads service configuration from environment variables.
package config

import (
	"os"
	"time"
)

type Config struct {
	DatabaseURL string
	AMQPURL     string
	RedisAddr   string
	Port        string

	// Delivery tuning (dispatch only).
	HTTPTimeout time.Duration
	MaxAttempts int
	Prefetch    int
}

func Load(defaultPort string) Config {
	return Config{
		DatabaseURL: getenv("DATABASE_URL", "postgres://relay:relay@localhost:5432/relay"),
		AMQPURL:     getenv("AMQP_URL", "amqp://relay:relay@localhost:5672/"),
		RedisAddr:   getenv("REDIS_ADDR", "localhost:6379"),
		Port:        getenv("PORT", defaultPort),
		HTTPTimeout: 10 * time.Second,
		MaxAttempts: 6,
		Prefetch:    16,
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
