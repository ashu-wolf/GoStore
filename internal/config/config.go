// Package config loads settings from environment variables.
package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds the server settings.
type Config struct {
	Addr            string
	DataDir         string
	ShutdownTimeout time.Duration
}

// Load reads the settings and applies defaults.
func Load() (Config, error) {
	cfg := Config{
		Addr:    env("GOSTORE_ADDR", "127.0.0.1:9000"),
		DataDir: env("GOSTORE_DATA_DIR", "./data"),
	}
	raw := env("GOSTORE_SHUTDOWN_TIMEOUT", "30s")
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return Config{}, fmt.Errorf("invalid GOSTORE_SHUTDOWN_TIMEOUT %q", raw)
	}
	cfg.ShutdownTimeout = d
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
