// Package config loads settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ashu-wolf/gostore/internal/domain"
)

const (
	defaultMaxObjectSize = int64(1 << 30) // 1 GiB
)

// Config holds the server settings.
type Config struct {
	Addr            string
	DataDir         string
	ShutdownTimeout time.Duration
	MaxObjectSize   int64
	ListPageSize    int
}

// Load reads the settings and applies defaults.
func Load() (Config, error) {
	cfg := Config{
		Addr:          env("GOSTORE_ADDR", "127.0.0.1:9000"),
		DataDir:       env("GOSTORE_DATA_DIR", "./data"),
		MaxObjectSize: defaultMaxObjectSize,
		ListPageSize:  domain.DefaultMaxKeys,
	}
	raw := env("GOSTORE_SHUTDOWN_TIMEOUT", "30s")
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return Config{}, fmt.Errorf("invalid GOSTORE_SHUTDOWN_TIMEOUT %q", raw)
	}
	cfg.ShutdownTimeout = d

	if rawMax := os.Getenv("GOSTORE_MAX_OBJECT_SIZE"); rawMax != "" {
		v, err := strconv.ParseInt(rawMax, 10, 64)
		if err != nil || v <= 0 {
			return Config{}, fmt.Errorf("invalid GOSTORE_MAX_OBJECT_SIZE %q", rawMax)
		}
		cfg.MaxObjectSize = v
	}
	if rawList := os.Getenv("GOSTORE_LIST_PAGE_SIZE"); rawList != "" {
		v, err := strconv.Atoi(rawList)
		if err != nil || v <= 0 || v > domain.DefaultMaxKeys {
			return Config{}, fmt.Errorf("invalid GOSTORE_LIST_PAGE_SIZE %q", rawList)
		}
		cfg.ListPageSize = v
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
