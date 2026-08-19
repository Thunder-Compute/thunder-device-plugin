package operator

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

func TestSyncPoolRefreshesOnlyAllocatedPool(t *testing.T) {
	ctx := context.Background()
	inventory := &fakeInventory{
		zones: []thunder.Zone{
			{ZoneID: "z1", DisplayName: "us-west-2a"},
			{ZoneID: "z2", DisplayName: "us-west-2b"},
		},
		nodes: map[string][]thunder.Server{
			"z1": {{GPUType: "A6000", GPUCount: 4, Status: "online"}},
			"z2": {{GPUType: "A6000", GPUCount: 5, Status: "online"}},
		},
	}
	kube := fake.NewSimpleClientset()
	op := New(testConfig(), kube, nil, inventory, nil)
	if err := op.Sync(ctx); err != nil {
		t.Fatalf("initial Sync: %v", err)
	}

	inventory.nodes["z1"] = []thunder.Server{{GPUType: "A6000", GPUCount: 2, Status: "online"}}
	inventory.nodes["z2"] = []thunder.Server{{GPUType: "A6000", GPUCount: 1, Status: "online"}}
	if err := op.SyncPool(ctx, "us-west-2a", "A6000"); err != nil {
		t.Fatalf("SyncPool: %v", err)
	}

	if got := capacityForPool(t, kube, "us-west-2a/a6000"); got != 2 {
		t.Fatalf("refreshed capacity = %d, want 2", got)
	}
	if got := capacityForPool(t, kube, "us-west-2b/a6000"); got != 5 {
		t.Fatalf("unrelated capacity = %d, want unchanged 5", got)
	}
}

func TestSyncPoolDeletesUnavailableAllocatedPool(t *testing.T) {
	ctx := context.Background()
	inventory := &fakeInventory{
		zones: []thunder.Zone{{ZoneID: "z1", DisplayName: "us-west-2a"}},
		nodes: map[string][]thunder.Server{
			"z1": {{GPUType: "A6000", GPUCount: 1, Status: "online"}},
		},
	}
	kube := fake.NewSimpleClientset()
	op := New(testConfig(), kube, nil, inventory, nil)
	if err := op.Sync(ctx); err != nil {
		t.Fatalf("initial Sync: %v", err)
	}
	inventory.nodes["z1"] = nil
	if err := op.SyncPool(ctx, "us-west-2a", "A6000"); err != nil {
		t.Fatalf("SyncPool: %v", err)
	}
	if got := capacityForPool(t, kube, "us-west-2a/a6000"); got != 0 {
		t.Fatalf("capacity = %d after disappearance, want 0", got)
	}
}

func capacityForPool(t *testing.T, kube *fake.Clientset, pool string) int64 {
	t.Helper()
	list, err := kube.ResourceV1().ResourceSlices().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list ResourceSlices: %v", err)
	}
	var capacity int64
	for _, slice := range list.Items {
		if slice.Spec.Pool.Name == pool {
			capacity += int64(len(slice.Spec.Devices))
		}
	}
	return capacity
}
