package operator

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/hostartifacts"
)

func TestSyncDeviceClassesUpdatesHostArtifactDefault(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	cfg := testConfig()
	op := New(cfg, kube, nil, &fakeInventory{}, nil)
	desired := map[poolKey]poolDefinition{
		{Zone: "us-west-2a", GPUType: "a6000"}: {Zone: "us-west-2a", GPUType: "a6000", Capacity: 1},
	}
	if err := op.syncDeviceClasses(ctx, desired); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	op.cfg.DefaultHostArtifactProfile = hostartifacts.ProfileFull
	if err := op.syncDeviceClasses(ctx, desired); err != nil {
		t.Fatalf("updated sync: %v", err)
	}
	class, err := kube.ResourceV1().DeviceClasses().Get(ctx, "thunder-gpu-a6000", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get DeviceClass: %v", err)
	}
	if len(class.Spec.Config) != 1 || class.Spec.Config[0].Opaque == nil {
		t.Fatalf("config = %#v", class.Spec.Config)
	}
	profile, err := hostartifacts.Decode(class.Spec.Config[0].Opaque.Parameters)
	if err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	if profile != hostartifacts.ProfileFull {
		t.Fatalf("profile = %q, want full", profile)
	}
}
