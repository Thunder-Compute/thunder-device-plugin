package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestFileCDIDeviceStoreCreateWritesMountsEnvAndHook(t *testing.T) {
	claimUID := types.UID("11111111-1111-1111-1111-111111111111")
	store := NewFileCDIDeviceStore(t.TempDir())
	store.LibCUDAPath = "/driver/lib/libcuda.so.1"
	store.LibNVMLPath = "/driver/nvml/libnvidia-ml.so.1"
	store.NVSMIPath = "/driver/bin/nvidia-smi"
	store.ClientInstallCommand = `echo install "${THUNDER_ENROLLMENT_TOKEN}"`

	qualifiedName, err := store.Create(context.Background(), Allocation{
		ClaimUID:       claimUID,
		ClaimNamespace: "default",
		ClaimName:      "claim-a",
		GPUType:        "A6000",
		GPUCount:       4,
	}, "raw-token-value")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if qualifiedName != "vgpu.thundercompute.com/vgpu=claim-11111111-1111-1111-1111-111111111111" {
		t.Fatalf("qualifiedName = %q", qualifiedName)
	}

	specData, err := os.ReadFile(store.specPath(qualifiedName))
	if err != nil {
		t.Fatalf("read CDI spec: %v", err)
	}
	if strings.Contains(string(specData), "raw-token-value") {
		t.Fatalf("CDI spec contains raw enrollment token: %s", specData)
	}

	var spec struct {
		Version string `json:"cdiVersion"`
		Kind    string `json:"kind"`
		Devices []struct {
			Name           string `json:"name"`
			ContainerEdits struct {
				Env    []string `json:"env"`
				Mounts []struct {
					HostPath      string   `json:"hostPath"`
					ContainerPath string   `json:"containerPath"`
					Type          string   `json:"type"`
					Options       []string `json:"options"`
				} `json:"mounts"`
				Hooks []struct {
					HookName string   `json:"hookName"`
					Path     string   `json:"path"`
					Args     []string `json:"args"`
					Env      []string `json:"env"`
					Timeout  int      `json:"timeout"`
				} `json:"hooks"`
			} `json:"containerEdits"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(specData, &spec); err != nil {
		t.Fatalf("decode CDI spec: %v", err)
	}
	if spec.Version != "0.6.0" || spec.Kind != DefaultCDIKind || len(spec.Devices) != 1 {
		t.Fatalf("unexpected CDI header/devices: %#v", spec)
	}
	edits := spec.Devices[0].ContainerEdits
	for _, want := range []string{
		"THUNDER_RESOURCE_CLAIM_UID=11111111-1111-1111-1111-111111111111",
		"THUNDER_RESOURCE_CLAIM_NAMESPACE=default",
		"THUNDER_RESOURCE_CLAIM_NAME=claim-a",
		"THUNDER_GPU_TYPE=A6000",
		"THUNDER_GPU_COUNT=4",
		"THUNDER_CDI_DEVICE=" + qualifiedName,
		"PATH=/driver/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LD_LIBRARY_PATH=/driver/lib:/driver/nvml",
	} {
		if !stringSliceContains(edits.Env, want) {
			t.Fatalf("env missing %q from %#v", want, edits.Env)
		}
	}
	if len(edits.Mounts) != 3 {
		t.Fatalf("mounts = %#v", edits.Mounts)
	}
	for _, want := range []string{"/driver/bin/nvidia-smi", "/driver/lib/libcuda.so.1", "/driver/nvml/libnvidia-ml.so.1"} {
		found := false
		for _, mount := range edits.Mounts {
			if mount.HostPath == want && mount.ContainerPath == want && mount.Type == "none" && stringSliceContains(mount.Options, "ro") && stringSliceContains(mount.Options, "bind") {
				found = true
			}
		}
		if !found {
			t.Fatalf("mount for %q missing from %#v", want, edits.Mounts)
		}
	}
	if len(edits.Hooks) != 1 {
		t.Fatalf("hooks = %#v", edits.Hooks)
	}
	hook := edits.Hooks[0]
	if hook.HookName != "createContainer" || hook.Path == "" || hook.Timeout != cdiHookTimeoutSeconds {
		t.Fatalf("hook = %#v", hook)
	}
	if !stringSliceContains(hook.Env, "THUNDER_RESOURCE_CLAIM_UID=11111111-1111-1111-1111-111111111111") {
		t.Fatalf("hook env = %#v", hook.Env)
	}

	hookData, err := os.ReadFile(hook.Path)
	if err != nil {
		t.Fatalf("read hook script: %v", err)
	}
	if strings.Contains(string(hookData), "raw-token-value") {
		t.Fatalf("hook script contains raw enrollment token: %s", hookData)
	}
	tokenData, err := os.ReadFile(store.tokenPath(cdiDeviceName(claimUID)))
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(tokenData) != "raw-token-value" {
		t.Fatalf("token file = %q", tokenData)
	}

	if err := store.Remove(context.Background(), qualifiedName); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(store.specPath(qualifiedName)); !os.IsNotExist(err) {
		t.Fatalf("spec still exists or unexpected error: %v", err)
	}
	if _, err := os.Stat(store.stateDir(cdiDeviceName(claimUID))); !os.IsNotExist(err) {
		t.Fatalf("state dir still exists or unexpected error: %v", err)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
