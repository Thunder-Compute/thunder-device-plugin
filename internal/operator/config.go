package operator

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDriverName    = "thundercompute.com"
	DefaultNamePrefix    = "thunder"
	DefaultZoneLabelKey  = "topology.kubernetes.io/zone"
	DefaultValidGPUCount = "1,2,4,8"
)

type Config struct {
	DriverName        string
	NamePrefix        string
	ZoneLabelKey      string
	ReconcileInterval time.Duration
	ValidGPUCounts    []string
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		DriverName:        envOrDefault("DRA_DRIVER_NAME", DefaultDriverName),
		NamePrefix:        envOrDefault("RESOURCE_SLICE_NAME_PREFIX", DefaultNamePrefix),
		ZoneLabelKey:      envOrDefault("NODE_ZONE_LABEL", DefaultZoneLabelKey),
		ReconcileInterval: 60 * time.Second,
		ValidGPUCounts:    splitCSV(envOrDefault("VALID_GPU_COUNTS", DefaultValidGPUCount)),
	}

	if raw := os.Getenv("RECONCILE_INTERVAL"); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse RECONCILE_INTERVAL: %w", err)
		}
		cfg.ReconcileInterval = interval
	}
	if cfg.DriverName == "" {
		return Config{}, fmt.Errorf("DRA_DRIVER_NAME is required")
	}
	if cfg.NamePrefix == "" {
		return Config{}, fmt.Errorf("RESOURCE_SLICE_NAME_PREFIX is required")
	}
	if cfg.ZoneLabelKey == "" {
		return Config{}, fmt.Errorf("NODE_ZONE_LABEL is required")
	}
	for _, value := range cfg.ValidGPUCounts {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("VALID_GPU_COUNTS must contain positive integers, got %q", value)
		}
	}
	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			values = append(values, value)
		}
	}
	return values
}
