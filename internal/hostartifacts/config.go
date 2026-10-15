package hostartifacts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	APIVersion = "thundercompute.com/v1alpha1"
	Kind       = "ThunderDeviceConfig"

	ProfileNone   Profile = "none"
	ProfileDriver Profile = "driver"
	ProfileFull   Profile = "full"
)

// Profile selects which administrator-configured host artifacts are exposed
// to a container. It deliberately does not contain host paths: workloads may
// select a profile, but only the cluster administrator controls its contents.
type Profile string

func ParseProfile(value string) (Profile, error) {
	profile := Profile(strings.TrimSpace(value))
	switch profile {
	case ProfileNone, ProfileDriver, ProfileFull:
		return profile, nil
	default:
		return "", fmt.Errorf("host artifact profile must be one of %q, %q, or %q, got %q",
			ProfileNone, ProfileDriver, ProfileFull, value)
	}
}

type Parameters struct {
	APIVersion    string               `json:"apiVersion"`
	Kind          string               `json:"kind"`
	HostArtifacts HostArtifactSettings `json:"hostArtifacts"`
}

type HostArtifactSettings struct {
	Profile Profile `json:"profile"`
}

func NewParameters(profile Profile) Parameters {
	return Parameters{
		APIVersion: APIVersion,
		Kind:       Kind,
		HostArtifacts: HostArtifactSettings{
			Profile: profile,
		},
	}
}

// DeviceConfiguration constructs the opaque DRA configuration published in a
// DeviceClass. Marshaling this fixed struct cannot fail.
func DeviceConfiguration(driver string, profile Profile) resourcev1.DeviceConfiguration {
	raw, err := json.Marshal(NewParameters(profile))
	if err != nil {
		panic(err)
	}
	return resourcev1.DeviceConfiguration{
		Opaque: &resourcev1.OpaqueDeviceConfiguration{
			Driver:     driver,
			Parameters: runtime.RawExtension{Raw: raw},
		},
	}
}

// Decode strictly validates the versioned payload. Unknown fields are errors
// so a misspelled security-relevant setting cannot be silently ignored.
func Decode(parameters runtime.RawExtension) (Profile, error) {
	raw := parameters.Raw
	if len(raw) == 0 && parameters.Object != nil {
		var err error
		raw, err = json.Marshal(parameters.Object)
		if err != nil {
			return "", fmt.Errorf("encode host artifact parameters: %w", err)
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", errors.New("host artifact parameters are empty")
	}

	var config Parameters
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return "", fmt.Errorf("decode host artifact parameters: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("decode host artifact parameters: multiple JSON values")
	}
	if config.APIVersion != APIVersion {
		return "", fmt.Errorf("unsupported host artifact apiVersion %q, want %q", config.APIVersion, APIVersion)
	}
	if config.Kind != Kind {
		return "", fmt.Errorf("unsupported host artifact kind %q, want %q", config.Kind, Kind)
	}
	profile, err := ParseProfile(string(config.HostArtifacts.Profile))
	if err != nil {
		return "", err
	}
	return profile, nil
}
