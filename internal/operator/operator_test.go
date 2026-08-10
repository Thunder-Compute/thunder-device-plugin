package operator

import (
	"context"
	"reflect"
	"sort"
	"strings"
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
		DriverName:             DefaultDriverName,
		NamePrefix:             DefaultNamePrefix,
		ZoneLabelKey:           DefaultZoneLabelKey,
		ReconcileInterval:      time.Minute,
		SharesPerGPU:           DefaultSharesPerGPU,
		DeviceClassPrefix:      DefaultDeviceClassPrefix,
		ExtendedResourcePrefix: DefaultExtendedResourcePrefix,
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

// gpuCapacity is the number of GPUs a pool publishes, which is now the number
// of devices rather than a capacity value on a single shared device.
func gpuCapacity(t *testing.T, slice *resourcev1.ResourceSlice) int64 {
	t.Helper()
	return int64(len(slice.Spec.Devices))
}

func TestBuildResourceSlicesPublishesOneDevicePerGPU(t *testing.T) {
	cfg := testConfig()
	definition := poolDefinition{Zone: "us-west-2a", GPUType: "A6000", Capacity: 3, HostCapacity: 3}

	slices := buildResourceSlices(cfg, definition, 1)
	if len(slices) != 1 {
		t.Fatalf("slice count = %d, want 1", len(slices))
	}
	devices := slices[0].Spec.Devices
	if len(devices) != 3 {
		t.Fatalf("device count = %d, want 3", len(devices))
	}

	names := make([]string, 0, len(devices))
	for _, device := range devices {
		names = append(names, device.Name)
		// Without oversubscription a GPU is exclusive, so it carries no
		// consumable capacity and needs no capacity feature gate.
		if device.Capacity != nil {
			t.Fatalf("device %s has capacity %v, want none", device.Name, device.Capacity)
		}
		if device.AllowMultipleAllocations != nil && *device.AllowMultipleAllocations {
			t.Fatalf("device %s allows multiple allocations without oversubscription", device.Name)
		}
	}
	if !reflect.DeepEqual(names, []string{"a6000-0", "a6000-1", "a6000-2"}) {
		t.Fatalf("device names = %v", names)
	}
	if slices[0].Spec.Pool.ResourceSliceCount != 1 {
		t.Fatalf("ResourceSliceCount = %d, want 1", slices[0].Spec.Pool.ResourceSliceCount)
	}
}

func TestBuildResourceSlicesOversubscribesWithShares(t *testing.T) {
	cfg := testConfig()
	cfg.SharesPerGPU = 3
	definition := poolDefinition{Zone: "us-west-2a", GPUType: "A6000", Capacity: 2, HostCapacity: 2}

	slices := buildResourceSlices(cfg, definition, 1)
	device := slices[0].Spec.Devices[0]

	if device.AllowMultipleAllocations == nil || !*device.AllowMultipleAllocations {
		t.Fatal("device does not allow multiple allocations")
	}
	capacity, ok := device.Capacity[resourcev1.QualifiedName(sharesCapacityName)]
	if !ok {
		t.Fatalf("device has no %s capacity", sharesCapacityName)
	}
	if capacity.Value.Value() != 3 {
		t.Fatalf("shares = %d, want 3", capacity.Value.Value())
	}
	// A claim takes exactly one share of each GPU it is allocated; more GPUs
	// means more devices, never a larger capacity request.
	if capacity.RequestPolicy == nil || capacity.RequestPolicy.Default.Value() != 1 {
		t.Fatalf("request policy default = %v, want 1", capacity.RequestPolicy)
	}
	if len(capacity.RequestPolicy.ValidValues) != 1 || capacity.RequestPolicy.ValidValues[0].Value() != 1 {
		t.Fatalf("valid values = %v, want [1]", capacity.RequestPolicy.ValidValues)
	}
}

func TestBuildResourceSlicesShardsLargePools(t *testing.T) {
	cfg := testConfig()
	// A slice holds at most 128 devices, so 300 GPUs need three shards.
	definition := poolDefinition{Zone: "us-west-2a", GPUType: "A6000", Capacity: 300, HostCapacity: 300}

	slices := buildResourceSlices(cfg, definition, 7)
	if len(slices) != 3 {
		t.Fatalf("slice count = %d, want 3", len(slices))
	}

	total := 0
	names := map[string]bool{}
	sliceNames := map[string]bool{}
	for _, slice := range slices {
		if len(slice.Spec.Devices) > devicesPerSlice {
			t.Fatalf("slice %s has %d devices, over the %d limit", slice.Name, len(slice.Spec.Devices), devicesPerSlice)
		}
		if slice.Spec.Pool.ResourceSliceCount != 3 {
			t.Fatalf("ResourceSliceCount = %d, want 3", slice.Spec.Pool.ResourceSliceCount)
		}
		if slice.Spec.Pool.Generation != 7 {
			t.Fatalf("generation = %d, want 7", slice.Spec.Pool.Generation)
		}
		if slice.Spec.Pool.Name != slices[0].Spec.Pool.Name {
			t.Fatalf("shards disagree on pool name: %q vs %q", slice.Spec.Pool.Name, slices[0].Spec.Pool.Name)
		}
		if sliceNames[slice.Name] {
			t.Fatalf("duplicate slice name %s", slice.Name)
		}
		sliceNames[slice.Name] = true
		for _, device := range slice.Spec.Devices {
			if names[device.Name] {
				t.Fatalf("duplicate device name %s across shards", device.Name)
			}
			names[device.Name] = true
			total++
		}
	}
	if total != 300 {
		t.Fatalf("total devices = %d, want 300", total)
	}
}

func TestBuildResourceSlicesTruncatesLongNames(t *testing.T) {
	cfg := testConfig()
	definition := poolDefinition{
		Zone:     strings.Repeat("very-long-zone-name", 5),
		GPUType:  strings.Repeat("gpu", 40),
		Capacity: 1,
	}

	slices := buildResourceSlices(cfg, definition, 1)
	if len(slices[0].Name) > 63 {
		t.Fatalf("slice name is %d chars: %s", len(slices[0].Name), slices[0].Name)
	}
	if name := slices[0].Spec.Devices[0].Name; len(name) > 63 {
		t.Fatalf("device name is %d chars: %s", len(name), name)
	}
}

func TestBuildDeviceClassPinsGPUType(t *testing.T) {
	cfg := testConfig()
	class := buildDeviceClass(cfg, "a6000")

	if class.Name != "thunder-gpu-a6000" {
		t.Fatalf("name = %q, want thunder-gpu-a6000", class.Name)
	}
	if got := derefString(class.Spec.ExtendedResourceName); got != "thundercompute.com/gpu-a6000" {
		t.Fatalf("extendedResourceName = %q", got)
	}
	if len(class.Spec.Selectors) != 1 || class.Spec.Selectors[0].CEL == nil {
		t.Fatalf("selectors = %#v", class.Spec.Selectors)
	}
	// The attribute is published upper-cased, so the selector must match that.
	expression := class.Spec.Selectors[0].CEL.Expression
	if !strings.Contains(expression, `"A6000"`) {
		t.Fatalf("selector does not pin the GPU type: %s", expression)
	}
	if !strings.Contains(expression, `device.driver == "`+DefaultDriverName+`"`) {
		t.Fatalf("selector does not pin the driver: %s", expression)
	}
}

func TestSyncDeviceClassesCreatesOnePerGPUTypeAndPrunes(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	op := New(testConfig(), kube, &fakeInventory{}, nil)

	desired := map[poolKey]poolDefinition{
		{Zone: "us-west-2a", GPUType: "a6000"}: {Zone: "us-west-2a", GPUType: "a6000", Capacity: 2},
		{Zone: "us-west-2b", GPUType: "a6000"}: {Zone: "us-west-2b", GPUType: "a6000", Capacity: 2},
		{Zone: "us-west-2a", GPUType: "h100"}:  {Zone: "us-west-2a", GPUType: "h100", Capacity: 1},
	}
	if err := op.syncDeviceClasses(ctx, desired); err != nil {
		t.Fatalf("syncDeviceClasses: %v", err)
	}

	// A GPU type is one class cluster-wide, not one per zone.
	names := deviceClassNames(t, kube)
	if !reflect.DeepEqual(names, []string{"thunder-gpu-a6000", "thunder-gpu-h100"}) {
		t.Fatalf("classes = %v", names)
	}

	// Losing a GPU type removes its class.
	delete(desired, poolKey{Zone: "us-west-2a", GPUType: "h100"})
	if err := op.syncDeviceClasses(ctx, desired); err != nil {
		t.Fatalf("syncDeviceClasses after shrink: %v", err)
	}
	if names := deviceClassNames(t, kube); !reflect.DeepEqual(names, []string{"thunder-gpu-a6000"}) {
		t.Fatalf("classes after shrink = %v", names)
	}
}

func TestSyncDeviceClassesLeavesUnmanagedClassesAlone(t *testing.T) {
	ctx := context.Background()
	// The chart ships its own catch-all class for ResourceClaims. It carries
	// the driver labels but not managed-by, and must survive reconciles.
	chartClass := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "thunder-gpu",
			Labels: map[string]string{
				"app.kubernetes.io/name":      driverAppName,
				"app.kubernetes.io/component": deviceClassComponent,
			},
		},
	}
	kube := fake.NewSimpleClientset(chartClass)
	op := New(testConfig(), kube, &fakeInventory{}, nil)

	if err := op.syncDeviceClasses(ctx, map[poolKey]poolDefinition{}); err != nil {
		t.Fatalf("syncDeviceClasses: %v", err)
	}
	if _, err := kube.ResourceV1().DeviceClasses().Get(ctx, "thunder-gpu", metav1.GetOptions{}); err != nil {
		t.Fatalf("chart DeviceClass was removed: %v", err)
	}
}

func TestSyncDeviceClassesDisabled(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	cfg := testConfig()
	cfg.ExtendedResourcePrefix = ""
	op := New(cfg, kube, &fakeInventory{}, nil)

	desired := map[poolKey]poolDefinition{
		{Zone: "us-west-2a", GPUType: "a6000"}: {Zone: "us-west-2a", GPUType: "a6000", Capacity: 2},
	}
	if err := op.syncDeviceClasses(ctx, desired); err != nil {
		t.Fatalf("syncDeviceClasses: %v", err)
	}
	if names := deviceClassNames(t, kube); len(names) != 0 {
		t.Fatalf("classes = %v, want none", names)
	}
}

func deviceClassNames(t *testing.T, kube *fake.Clientset) []string {
	t.Helper()
	list, err := kube.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list DeviceClasses: %v", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	return names
}
