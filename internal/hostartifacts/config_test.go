package hostartifacts

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestDecode(t *testing.T) {
	for _, profile := range []Profile{ProfileNone, ProfileDriver, ProfileFull} {
		t.Run(string(profile), func(t *testing.T) {
			configuration := DeviceConfiguration("thundercompute.com", profile)
			got, err := Decode(configuration.Opaque.Parameters)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got != profile {
				t.Fatalf("profile = %q, want %q", got, profile)
			}
		})
	}
}

func TestDecodeRejectsInvalidConfiguration(t *testing.T) {
	tests := map[string]string{
		"unknown field": `{"apiVersion":"thundercompute.com/v1alpha1","kind":"ThunderDeviceConfig","hostArtifacts":{"profile":"driver","profiel":"full"}}`,
		"wrong version": `{"apiVersion":"thundercompute.com/v2","kind":"ThunderDeviceConfig","hostArtifacts":{"profile":"driver"}}`,
		"wrong kind":    `{"apiVersion":"thundercompute.com/v1alpha1","kind":"OtherConfig","hostArtifacts":{"profile":"driver"}}`,
		"bad profile":   `{"apiVersion":"thundercompute.com/v1alpha1","kind":"ThunderDeviceConfig","hostArtifacts":{"profile":"everything"}}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(runtime.RawExtension{Raw: []byte(raw)}); err == nil {
				t.Fatal("Decode accepted invalid configuration")
			} else if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("Decode returned an empty error")
			}
		})
	}
}
