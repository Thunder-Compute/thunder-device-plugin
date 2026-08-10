package daemon

import (
	"context"
	"encoding/json"
	"github.com/Thunder-Compute/thunder-device-plugin/internal/thunderclient"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
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
	if qualifiedName != "thundercompute.com/gpu=claim-11111111-1111-1111-1111-111111111111" {
		t.Fatalf("qualifiedName = %q", qualifiedName)
	}

	specData, err := os.ReadFile(store.specPath(qualifiedName))
	if err != nil {
		t.Fatalf("read CDI spec: %v", err)
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
		ThunderEnrollmentTokenEnv + "=raw-token-value",
		ThunderClientInstallCommandEnv + "=" + store.ClientInstallCommand,
		"THUNDER_CDI_DEVICE=" + qualifiedName,
		"LD_PRELOAD=/etc/thunder/libthunder.so",
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
	if len(edits.Hooks) != 0 {
		t.Fatalf("hooks = %#v", edits.Hooks)
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

func TestKubernetesGuestConfigStoreCreateWritesConfigMapAndSecret(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	store := NewKubernetesGuestConfigStore(kube)
	allocation := Allocation{
		ClaimUID:       types.UID("11111111-1111-1111-1111-111111111111"),
		ClaimNamespace: "default",
		ClaimName:      "claim-a",
		GPUType:        "A6000",
		GPUCount:       2,
	}

	artifacts, err := store.Create(ctx, allocation, "raw-token-value", `echo install "${THUNDER_ENROLLMENT_TOKEN}"`)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if artifacts.Namespace != "default" || artifacts.SecretName != "claim-a-thunder-setup" {
		t.Fatalf("artifacts = %#v", artifacts)
	}

	secret, err := kube.CoreV1().Secrets("default").Get(ctx, artifacts.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Secret: %v", err)
	}
	script := string(secret.Data[ThunderGuestInstallScriptKey])
	if !strings.Contains(script, `echo install "${THUNDER_ENROLLMENT_TOKEN}"`) {
		t.Fatalf("script does not run the install command:\n%s", script)
	}
	// The guest becomes a Thunder client, which needs no NVIDIA driver: the
	// driver lives on the GPU server, and libthunder carries CUDA to it. The
	// script stays distribution agnostic and adds nothing to VM boot time.
	for _, unwanted := range []string{
		"cuda-keyring", "nvidia-driver-pinning", "cuda-drivers", "apt-get", "dpkg",
	} {
		if strings.Contains(script, unwanted) {
			t.Fatalf("script installs %q, which a Thunder client does not need:\n%s", unwanted, script)
		}
	}
	if strings.Contains(script, "raw-token-value") {
		t.Fatalf("script contains raw token: %s", script)
	}

	if got := string(secret.Data[ThunderGuestSecretTokenKey]); got != "raw-token-value" {
		t.Fatalf("secret token = %q", got)
	}

	if _, err := store.Create(ctx, allocation, "new-token-value", `echo updated`); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	secret, err = kube.CoreV1().Secrets("default").Get(ctx, artifacts.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated Secret: %v", err)
	}
	if got := string(secret.Data[ThunderGuestSecretTokenKey]); got != "new-token-value" {
		t.Fatalf("updated secret token = %q", got)
	}

	if err := store.Remove(ctx, artifacts); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := store.Remove(ctx, artifacts); err != nil {
		t.Fatalf("idempotent Remove: %v", err)
	}
}

func TestUpsertHoldsTheClientWithAFinalizer(t *testing.T) {
	ctx := context.Background()
	store, dyn := newTestClientStore(t)

	client := ThunderClient{ClaimUID: types.UID("uid-1"), ClaimName: "pod-gpu", ClaimNamespace: "default"}
	if err := store.Upsert(ctx, client); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	created := getThunderClient(t, dyn, ThunderClientName(client.ClaimUID))
	if got := created.GetFinalizers(); len(got) != 1 || got[0] != thunderclient.Finalizer {
		t.Fatalf("finalizers = %v, want [%s]", got, thunderclient.Finalizer)
	}

	// An update replaces metadata wholesale, so the finalizer has to survive it
	// and anything else on the resource has to be left alone.
	created.SetFinalizers([]string{"example.com/other", thunderclient.Finalizer})
	if _, err := dyn.Resource(ThunderClientGVR).Namespace("thunder-system").
		Update(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("seed extra finalizer: %v", err)
	}
	client.GPUType = "A6000"
	if err := store.Upsert(ctx, client); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if got := getThunderClient(t, dyn, ThunderClientName(client.ClaimUID)).GetFinalizers(); len(got) != 2 {
		t.Fatalf("finalizers = %v, want both kept", got)
	}
}

func TestDeleteReleasesTheFinalizerSoTheClientCanGo(t *testing.T) {
	ctx := context.Background()
	store, dyn := newTestClientStore(t)

	client := ThunderClient{ClaimUID: types.UID("uid-1"), ClaimName: "pod-gpu", ClaimNamespace: "default"}
	if err := store.Upsert(ctx, client); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.Delete(ctx, client.ClaimUID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := dyn.Resource(ThunderClientGVR).Namespace("thunder-system").
		Get(ctx, ThunderClientName(client.ClaimUID), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("client still present after Delete: %v", err)
	}
}

func newTestClientStore(t *testing.T) (*KubernetesThunderClientStore, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		ThunderClientGVR.GroupVersion().WithKind("ThunderClientList"),
		&unstructured.UnstructuredList{},
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{ThunderClientGVR: "ThunderClientList"})
	return NewKubernetesThunderClientStore(dyn, "thunder-system"), dyn
}

func getThunderClient(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := dyn.Resource(ThunderClientGVR).Namespace("thunder-system").
		Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return obj
}
