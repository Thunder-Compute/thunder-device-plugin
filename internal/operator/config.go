package operator

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	DefaultDriverName   = "thundercompute.com"
	DefaultNamePrefix   = "thunder"
	DefaultZoneLabelKey = "topology.kubernetes.io/zone"

	// DefaultDeviceClassPrefix and DefaultExtendedResourcePrefix name the
	// per-GPU-type DeviceClasses the operator generates, so a workload can ask
	// for a specific model by resource name.
	DefaultDeviceClassPrefix      = "thunder-gpu-"
	DefaultExtendedResourcePrefix = "thundercompute.com/gpu-"
)

type Config struct {
	DriverName        string
	NamePrefix        string
	ZoneLabelKey      string
	ReconcileInterval time.Duration
	// DeviceClassPrefix and ExtendedResourcePrefix name the per-GPU-type
	// DeviceClasses. An empty ExtendedResourcePrefix disables them.
	DeviceClassPrefix      string
	ExtendedResourcePrefix string
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		DriverName:        envOrDefault("DRA_DRIVER_NAME", DefaultDriverName),
		NamePrefix:        envOrDefault("RESOURCE_SLICE_NAME_PREFIX", DefaultNamePrefix),
		ZoneLabelKey:      envOrDefault("NODE_ZONE_LABEL", DefaultZoneLabelKey),
		ReconcileInterval: 60 * time.Second,
		DeviceClassPrefix: envOrDefault("DEVICE_CLASS_PREFIX", DefaultDeviceClassPrefix),
		// Explicitly empty disables per-GPU-type classes, so only look at the
		// default when the variable is unset.
		ExtendedResourcePrefix: envOrDefaultAllowEmpty("EXTENDED_RESOURCE_PREFIX", DefaultExtendedResourcePrefix),
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
	if cfg.ExtendedResourcePrefix != "" {
		if cfg.DeviceClassPrefix == "" {
			return Config{}, fmt.Errorf("DEVICE_CLASS_PREFIX is required when EXTENDED_RESOURCE_PREFIX is set")
		}
		if !strings.Contains(cfg.ExtendedResourcePrefix, "/") {
			return Config{}, fmt.Errorf("EXTENDED_RESOURCE_PREFIX must be a domain-qualified prefix such as %q, got %q",
				DefaultExtendedResourcePrefix, cfg.ExtendedResourcePrefix)
		}
	}
	return cfg, nil
}

// envOrDefaultAllowEmpty treats an explicitly empty variable as a real value,
// so a setting can be switched off rather than falling back to its default.
func envOrDefaultAllowEmpty(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
