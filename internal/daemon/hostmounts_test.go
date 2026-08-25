package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/hostartifacts"
)

func TestFileCDIDeviceStoreMountProfiles(t *testing.T) {
	store := NewFileCDIDeviceStore(t.TempDir())
	store.LibCUDAPath = "/driver/lib/libcuda.so.1"
	store.LibNVMLPath = "/driver/nvml/libnvidia-ml.so.1"
	store.NVSMIPath = "/driver/bin/nvidia-smi"
	store.ToolkitPath = "/opt/toolkit"

	tests := []struct {
		profile hostartifacts.Profile
		mounts  int
	}{
		{profile: hostartifacts.ProfileNone, mounts: 0},
		{profile: hostartifacts.ProfileDriver, mounts: 3},
		{profile: hostartifacts.ProfileFull, mounts: 4},
	}
	for _, test := range tests {
		t.Run(string(test.profile), func(t *testing.T) {
			mounts := store.mounts(test.profile)
			if len(mounts) != test.mounts {
				t.Fatalf("mounts = %#v, want %d", mounts, test.mounts)
			}
			env := store.containerEnv("thundercompute.com/gpu=test", Allocation{HostArtifactProfile: test.profile}, "token")
			joined := strings.Join(env, "\n")
			if test.profile == hostartifacts.ProfileNone && strings.Contains(joined, "LD_LIBRARY_PATH=") {
				t.Fatalf("none profile contains host library path: %s", joined)
			}
			if test.profile == hostartifacts.ProfileFull && (!strings.Contains(joined, "/opt/toolkit/bin") || !strings.Contains(joined, "/opt/toolkit/lib64")) {
				t.Fatalf("full profile omits toolkit environment: %s", joined)
			}
		})
	}
}

func TestFileCDIDeviceStoreRejectsMissingToolkitBeforeWritingState(t *testing.T) {
	hostRoot := t.TempDir()
	store := NewFileCDIDeviceStore(t.TempDir())
	store.HostRoot = hostRoot
	store.LibCUDAPath = "/driver/lib/libcuda.so.1"
	store.LibNVMLPath = "/driver/lib/libnvidia-ml.so.1"
	store.NVSMIPath = "/driver/bin/nvidia-smi"
	store.ToolkitPath = "/toolkit"
	store.ClientInstallCommand = "install"
	for _, path := range []string{store.LibCUDAPath, store.LibNVMLPath, store.NVSMIPath} {
		resolved := resolveNodePath(hostRoot, path)
		if err := os.MkdirAll(filepath.Dir(resolved), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(resolved, []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	uid := types.UID("11111111-1111-1111-1111-111111111111")
	_, err := store.Create(context.Background(), Allocation{
		ClaimUID:            uid,
		HostArtifactProfile: hostartifacts.ProfileFull,
	}, "token")
	if err == nil || !strings.Contains(err.Error(), "/toolkit") {
		t.Fatalf("Create error = %v, want missing toolkit path", err)
	}
	if _, err := os.Stat(store.stateDir(cdiDeviceName(uid))); !os.IsNotExist(err) {
		t.Fatalf("state was written before validation: %v", err)
	}

	if err := os.WriteFile(resolveNodePath(hostRoot, store.ToolkitPath), []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), Allocation{ClaimUID: uid, HostArtifactProfile: hostartifacts.ProfileFull}, "token"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Create with toolkit file error = %v", err)
	}
}
