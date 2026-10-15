package daemon

import (
	"testing"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/hostartifacts"
)

func TestConfigFromLookupHostArtifacts(t *testing.T) {
	base := map[string]string{
		EnvNode:               "node-a",
		EnvMinNVDriverVersion: "610",
		EnvThunderAPIToken:    "token",
	}
	cfg, err := configFromLookup(mapLookup(base))
	if err != nil {
		t.Fatalf("configFromLookup: %v", err)
	}
	if cfg.HostArtifactProfile != hostartifacts.ProfileDriver || cfg.HostArtifactToolkit != DefaultHostArtifactToolkit {
		t.Fatalf("host artifact defaults = %q, %q", cfg.HostArtifactProfile, cfg.HostArtifactToolkit)
	}

	custom := map[string]string{}
	for key, value := range base {
		custom[key] = value
	}
	custom[EnvHostArtifactProfile] = "full"
	custom[EnvHostArtifactToolkit] = "/opt/cuda"
	cfg, err = configFromLookup(mapLookup(custom))
	if err != nil {
		t.Fatalf("custom configFromLookup: %v", err)
	}
	if cfg.HostArtifactProfile != hostartifacts.ProfileFull || cfg.HostArtifactToolkit != "/opt/cuda" {
		t.Fatalf("custom host artifacts = %q, %q", cfg.HostArtifactProfile, cfg.HostArtifactToolkit)
	}

	custom[EnvHostArtifactProfile] = "typo"
	if _, err := configFromLookup(mapLookup(custom)); err == nil {
		t.Fatal("configFromLookup accepted invalid host artifact profile")
	}
}
