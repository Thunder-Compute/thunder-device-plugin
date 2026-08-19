package daemon

import (
	"testing"

	resourcev1 "k8s.io/api/resource/v1"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/hostartifacts"
)

func allocationConfig(source resourcev1.AllocationConfigSource, profile hostartifacts.Profile, requests ...string) resourcev1.DeviceAllocationConfiguration {
	return resourcev1.DeviceAllocationConfiguration{
		Source:              source,
		Requests:            requests,
		DeviceConfiguration: hostartifacts.DeviceConfiguration(DefaultDriverName, profile),
	}
}

func TestResolveHostArtifactProfileClaimOverridesClass(t *testing.T) {
	driver := &Driver{DefaultHostArtifactProfile: hostartifacts.ProfileDriver}
	allocation := Allocation{Devices: []AllocatedDevice{{RequestName: "gpu"}}}
	configs := []resourcev1.DeviceAllocationConfiguration{
		allocationConfig(resourcev1.AllocationConfigSourceClass, hostartifacts.ProfileNone, "gpu"),
		allocationConfig(resourcev1.AllocationConfigSourceClaim, hostartifacts.ProfileFull, "gpu"),
	}
	if err := driver.resolveHostArtifactProfile(&allocation, configs); err != nil {
		t.Fatalf("resolveHostArtifactProfile: %v", err)
	}
	if allocation.HostArtifactProfile != hostartifacts.ProfileFull {
		t.Fatalf("profile = %q, want full", allocation.HostArtifactProfile)
	}
}

func TestResolveHostArtifactProfileUsesDefaultAndMainRequestTarget(t *testing.T) {
	driver := &Driver{DefaultHostArtifactProfile: hostartifacts.ProfileDriver}
	allocation := Allocation{Devices: []AllocatedDevice{{RequestName: "gpu/first"}}}
	if err := driver.resolveHostArtifactProfile(&allocation, nil); err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if allocation.HostArtifactProfile != hostartifacts.ProfileDriver {
		t.Fatalf("default profile = %q, want driver", allocation.HostArtifactProfile)
	}
	if err := driver.resolveHostArtifactProfile(&allocation, []resourcev1.DeviceAllocationConfiguration{
		allocationConfig(resourcev1.AllocationConfigSourceClaim, hostartifacts.ProfileNone, "gpu"),
	}); err != nil {
		t.Fatalf("resolve main request: %v", err)
	}
	if allocation.HostArtifactProfile != hostartifacts.ProfileNone {
		t.Fatalf("targeted profile = %q, want none", allocation.HostArtifactProfile)
	}
}

func TestResolveHostArtifactProfileRejectsMixedRequests(t *testing.T) {
	driver := &Driver{DefaultHostArtifactProfile: hostartifacts.ProfileDriver}
	allocation := Allocation{Devices: []AllocatedDevice{{RequestName: "gpu-a"}, {RequestName: "gpu-b"}}}
	err := driver.resolveHostArtifactProfile(&allocation, []resourcev1.DeviceAllocationConfiguration{
		allocationConfig(resourcev1.AllocationConfigSourceClaim, hostartifacts.ProfileNone, "gpu-a"),
	})
	if err == nil {
		t.Fatal("resolveHostArtifactProfile accepted different profiles for one claim")
	}
}

func TestResolveHostArtifactProfileRejectsConflictingClaimEntries(t *testing.T) {
	driver := &Driver{DefaultHostArtifactProfile: hostartifacts.ProfileDriver}
	allocation := Allocation{Devices: []AllocatedDevice{{RequestName: "gpu"}}}
	err := driver.resolveHostArtifactProfile(&allocation, []resourcev1.DeviceAllocationConfiguration{
		allocationConfig(resourcev1.AllocationConfigSourceClaim, hostartifacts.ProfileNone),
		allocationConfig(resourcev1.AllocationConfigSourceClaim, hostartifacts.ProfileFull),
	})
	if err == nil {
		t.Fatal("resolveHostArtifactProfile accepted conflicting claim entries")
	}
}
