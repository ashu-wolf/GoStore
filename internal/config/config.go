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
	Addr             string
	DataDir          string
	ShutdownTimeout  time.Duration
	MaxObjectSize    int64
	MaxMultipartSize int64
	MinPartSize      int64
	MaxUploadParts   int
	UploadExpiry     time.Duration
	ListPageSize     int
	AccessKey        string
	SecretKey        string
	Region           string
}

// Load reads the settings and applies defaults.
func Load() (Config, error) {
	cfg := Config{
		Addr:             env("GOSTORE_ADDR", "127.0.0.1:9000"),
		DataDir:          env("GOSTORE_DATA_DIR", "./data"),
		MaxObjectSize:    defaultMaxObjectSize,
		MaxMultipartSize: domain.DefaultMaxMultipartObjectSize,
		MinPartSize:      domain.DefaultMinPartSize,
		MaxUploadParts:   domain.DefaultMaxUploadParts,
		UploadExpiry:     domain.DefaultUploadExpiry,
		ListPageSize:     domain.DefaultMaxKeys,
		AccessKey:        os.Getenv("GOSTORE_ACCESS_KEY"),
		SecretKey:        os.Getenv("GOSTORE_SECRET_KEY"),
		Region:           env("GOSTORE_REGION", "us-east-1"),
	}
	if cfg.AccessKey == "" {
		return Config{}, fmt.Errorf("GOSTORE_ACCESS_KEY is required")
	}
	if cfg.SecretKey == "" {
		return Config{}, fmt.Errorf("GOSTORE_SECRET_KEY is required")
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
	if rawMP := os.Getenv("GOSTORE_MAX_MULTIPART_SIZE"); rawMP != "" {
		v, err := strconv.ParseInt(rawMP, 10, 64)
		if err != nil || v <= 0 {
			return Config{}, fmt.Errorf("invalid GOSTORE_MAX_MULTIPART_SIZE %q", rawMP)
		}
		cfg.MaxMultipartSize = v
	}
	if rawMin := os.Getenv("GOSTORE_MIN_PART_SIZE"); rawMin != "" {
		v, err := strconv.ParseInt(rawMin, 10, 64)
		if err != nil || v <= 0 {
			return Config{}, fmt.Errorf("invalid GOSTORE_MIN_PART_SIZE %q", rawMin)
		}
		cfg.MinPartSize = v
	}
	if rawParts := os.Getenv("GOSTORE_MAX_UPLOAD_PARTS"); rawParts != "" {
		v, err := strconv.Atoi(rawParts)
		if err != nil || v <= 0 || v > domain.DefaultMaxUploadParts {
			return Config{}, fmt.Errorf("invalid GOSTORE_MAX_UPLOAD_PARTS %q", rawParts)
		}
		cfg.MaxUploadParts = v
	}
	if rawExp := os.Getenv("GOSTORE_UPLOAD_EXPIRY"); rawExp != "" {
		exp, err := time.ParseDuration(rawExp)
		if err != nil || exp <= 0 {
			return Config{}, fmt.Errorf("invalid GOSTORE_UPLOAD_EXPIRY %q", rawExp)
		}
		cfg.UploadExpiry = exp
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
