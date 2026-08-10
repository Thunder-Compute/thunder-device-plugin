package operator

import (
	"context"
	"reflect"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

type fakeInventory struct {
	zones   []thunder.Zone
	nodes   map[string][]thunder.Server
	clients map[string][]thunder.RegisteredClient
}

func (f *fakeInventory) ListZones(context.Context) ([]thunder.Zone, error) {
	return append([]thunder.Zone(nil), f.zones...), nil
}

func (f *fakeInventory) ListServers(_ context.Context, zoneID string) ([]thunder.Server, error) {
	return append([]thunder.Server(nil), f.nodes[zoneID]...), nil
}

func (f *fakeInventory) ListClients(_ context.Context, zoneID string) ([]thunder.RegisteredClient, error) {
	return append([]thunder.RegisteredClient(nil), f.clients[zoneID]...), nil
}

func testConfig() Config {
	return Config{
		DriverName:        DefaultDriverName,
		NamePrefix:        DefaultNamePrefix,
		ZoneLabelKey:      DefaultZoneLabelKey,
		ReconcileInterval: time.Minute,
		ValidGPUCounts:    []string{"1", "2", "4", "8"},
	}
}

func TestBuildDesiredPoolsUsesMaxHostAndClientCapacity(t *testing.T) {
	inventory := &fakeInventory{
		zones: []thunder.Zone{{ZoneID: "zone-1", DisplayName: "us-west-2a"}},
		nodes: map[string][]thunder.Server{
			"zone-1": {
				{GPUType: "A6000", GPUCount: 10, Status: "active"},
				{GPUType: "A6000", GPUCount: 99, Status: "offline"},
			},
		},
		clients: map[string][]thunder.RegisteredClient{
			"zone-1": {
				{GPUType: "a6000", GPUCount: 12},
				{GPUType: "H100", GPUCount: 2},
			},
		},
	}

	pools, err := buildDesiredPools(context.Background(), inventory)
	if err != nil {
		t.Fatalf("buildDesiredPools returned error: %v", err)
	}

	a6000 := pools[poolKey{Zone: "us-west-2a", GPUType: "a6000"}]
	if a6000.HostCapacity != 10 || a6000.ClientCapacity != 12 || a6000.Capacity != 12 {
		t.Fatalf("unexpected a6000 pool: %#v", a6000)
	}
	h100 := pools[poolKey{Zone: "us-west-2a", GPUType: "h100"}]
	if h100.HostCapacity != 0 || h100.ClientCapacity != 2 || h100.Capacity != 2 {
		t.Fatalf("unexpected h100 pool: %#v", h100)
	}
}

func TestSyncCreatesUpdatesAndDeletesResourceSlices(t *testing.T) {
	ctx := context.Background()
	inventory := &fakeInventory{
		zones: []thunder.Zone{{ZoneID: "zone-1", DisplayName: "us-west-2a"}},
		nodes: map[string][]thunder.Server{
			"zone-1": {{GPUType: "A6000", GPUCount: 4, Status: "online"}},
		},
		clients: map[string][]thunder.RegisteredClient{},
	}
	kube := fake.NewSimpleClientset()
	op := New(testConfig(), kube, inventory, nil)

	if err := op.Sync(ctx); err != nil {
		t.Fatalf("initial Sync returned error: %v", err)
	}
	slice := getOnlyResourceSlice(t, kube)
	if slice.Spec.Pool.Generation != 1 {
		t.Fatalf("generation = %d, want 1", slice.Spec.Pool.Generation)
	}
	if got := gpuCapacity(t, slice); got != 4 {
		t.Fatalf("capacity = %d, want 4", got)
	}

	inventory.nodes["zone-1"] = []thunder.Server{{GPUType: "A6000", GPUCount: 6, Status: "online"}}
	if err := op.Sync(ctx); err != nil {
		t.Fatalf("second Sync returned error: %v", err)
	}
	slice = getOnlyResourceSlice(t, kube)
	if slice.Spec.Pool.Generation != 2 {
		t.Fatalf("generation after capacity change = %d, want 2", slice.Spec.Pool.Generation)
	}
	if got := gpuCapacity(t, slice); got != 6 {
		t.Fatalf("capacity after update = %d, want 6", got)
	}

	if err := op.Sync(ctx); err != nil {
		t.Fatalf("third Sync returned error: %v", err)
	}
	slice = getOnlyResourceSlice(t, kube)
	if slice.Spec.Pool.Generation != 2 {
		t.Fatalf("generation after no-op = %d, want 2", slice.Spec.Pool.Generation)
	}

	inventory.nodes["zone-1"] = nil
	if err := op.Sync(ctx); err != nil {
		t.Fatalf("delete Sync returned error: %v", err)
	}
	list, err := kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list ResourceSlices: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("ResourceSlices remaining after capacity disappeared = %d, want 0", len(list.Items))
	}
}

func getOnlyResourceSlice(t *testing.T, kube *fake.Clientset) *resourcev1.ResourceSlice {
	t.Helper()
	list, err := kube.ResourceV1().ResourceSlices().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list ResourceSlices: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("ResourceSlice count = %d, want 1", len(list.Items))
	}
	return &list.Items[0]
}

func gpuCapacity(t *testing.T, slice *resourcev1.ResourceSlice) int64 {
	t.Helper()
	if len(slice.Spec.Devices) != 1 {
		t.Fatalf("device count = %d, want 1", len(slice.Spec.Devices))
	}
	capacity, ok := slice.Spec.Devices[0].Capacity[resourcev1.QualifiedName(gpuCountCapacityName)]
	if !ok {
		t.Fatalf("gpu count capacity is missing")
	}
	return capacity.Value.Value()
}

func TestCapacityRequestPolicyClampsToCapacity(t *testing.T) {
	// The API server rejects any valid value larger than the device capacity,
	// and rejects a default that is not itself a valid value.
	tests := []struct {
		name        string
		counts      []string
		capacity    int64
		wantValid   []string
		wantDefault string
		wantNil     bool
	}{
		{
			name:        "all options fit",
			counts:      []string{"1", "2", "4"},
			capacity:    4,
			wantValid:   []string{"1", "2", "4"},
			wantDefault: "1",
		},
		{
			name:        "drops options larger than capacity",
			counts:      []string{"1", "2", "4", "8"},
			capacity:    4,
			wantValid:   []string{"1", "2", "4"},
			wantDefault: "1",
		},
		{
			name:        "default is the smallest surviving option",
			counts:      []string{"2", "4", "8"},
			capacity:    4,
			wantValid:   []string{"2", "4"},
			wantDefault: "2",
		},
		{
			name:     "no option fits",
			counts:   []string{"4", "8"},
			capacity: 2,
			wantNil:  true,
		},
		{
			name:        "unsorted input is ordered",
			counts:      []string{"4", "1", "2"},
			capacity:    8,
			wantValid:   []string{"1", "2", "4"},
			wantDefault: "1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := capacityRequestPolicy(test.counts, test.capacity)
			if test.wantNil {
				if policy != nil {
					t.Fatalf("policy = %#v, want nil", policy)
				}
				return
			}
			if policy == nil {
				t.Fatal("policy = nil, want a policy")
			}
			got := make([]string, 0, len(policy.ValidValues))
			for _, value := range policy.ValidValues {
				got = append(got, value.String())
			}
			if !reflect.DeepEqual(got, test.wantValid) {
				t.Fatalf("ValidValues = %v, want %v", got, test.wantValid)
			}
			if policy.Default == nil || policy.Default.String() != test.wantDefault {
				t.Fatalf("Default = %v, want %s", policy.Default, test.wantDefault)
			}
			for _, value := range policy.ValidValues {
				if value.Value() > test.capacity {
					t.Fatalf("ValidValues contains %s, larger than capacity %d", value.String(), test.capacity)
				}
			}
		})
	}
}
