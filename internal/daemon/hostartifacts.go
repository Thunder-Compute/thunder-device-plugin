package daemon

import (
	"fmt"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/hostartifacts"
)

// resolveHostArtifactProfile applies DRA configuration to each allocated
// request. A claim entry overrides a class entry, while conflicting entries at
// the same source are rejected. The final profile must be claim-wide because
// this driver returns one CDI device for the entire claim.
func (d *Driver) resolveHostArtifactProfile(allocation *Allocation, configs []resourcev1.DeviceAllocationConfiguration) error {
	defaultProfile := d.DefaultHostArtifactProfile
	if defaultProfile == "" {
		defaultProfile = hostartifacts.ProfileDriver
	}
	if _, err := hostartifacts.ParseProfile(string(defaultProfile)); err != nil {
		return fmt.Errorf("invalid default host artifact profile: %w", err)
	}

	requests := make(map[string]struct{}, len(allocation.Devices))
	for _, device := range allocation.Devices {
		requests[device.RequestName] = struct{}{}
	}

	var resolved hostartifacts.Profile
	for request := range requests {
		profile, err := d.profileForRequest(request, defaultProfile, configs)
		if err != nil {
			return err
		}
		if resolved != "" && resolved != profile {
			return fmt.Errorf("allocated requests resolve to different host artifact profiles: %q and %q", resolved, profile)
		}
		resolved = profile
	}
	allocation.HostArtifactProfile = resolved
	return nil
}

func (d *Driver) profileForRequest(request string, fallback hostartifacts.Profile, configs []resourcev1.DeviceAllocationConfiguration) (hostartifacts.Profile, error) {
	profiles := map[resourcev1.AllocationConfigSource]hostartifacts.Profile{}
	for _, config := range configs {
		if !configurationApplies(config.Requests, request) || config.Opaque == nil || config.Opaque.Driver != d.driverName() {
			continue
		}
		if config.Source != resourcev1.AllocationConfigSourceClass && config.Source != resourcev1.AllocationConfigSourceClaim {
			return "", fmt.Errorf("host artifact configuration for request %q has unsupported source %q", request, config.Source)
		}
		profile, err := hostartifacts.Decode(config.Opaque.Parameters)
		if err != nil {
			return "", fmt.Errorf("host artifact configuration for request %q: %w", request, err)
		}
		if previous, exists := profiles[config.Source]; exists && previous != profile {
			return "", fmt.Errorf("request %q has conflicting %s host artifact profiles %q and %q", request, config.Source, previous, profile)
		}
		profiles[config.Source] = profile
	}
	if profile, exists := profiles[resourcev1.AllocationConfigSourceClaim]; exists {
		return profile, nil
	}
	if profile, exists := profiles[resourcev1.AllocationConfigSourceClass]; exists {
		return profile, nil
	}
	return fallback, nil
}

func configurationApplies(configRequests []string, allocatedRequest string) bool {
	if len(configRequests) == 0 {
		return true
	}
	for _, request := range configRequests {
		if request == allocatedRequest || (!strings.Contains(request, "/") && strings.HasPrefix(allocatedRequest, request+"/")) {
			return true
		}
	}
	return false
}
