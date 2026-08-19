package operator

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	thunder "github.com/Thunder-Compute/thunder-sdk"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/hostartifacts"
)

type fakeInventory struct {
	revoked    []string
	revokeErr  error
	zones      []thunder.Zone
	nodes      map[string][]thunder.Server
	clients    map[string][]thunder.RegisteredClient
	targets    map[string]thunder.ZoneOversubscriptionTargetsResponse
	targetsErr error
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

func (f *fakeInventory) UnenrollClient(_ context.Context, enrollmentTokenID string) (thunder.DeleteEnrollmentServerResponse, error) {
	if f.revokeErr != nil {
		return thunder.DeleteEnrollmentServerResponse{}, f.revokeErr
	}
	f.revoked = append(f.revoked, enrollmentTokenID)
	return thunder.DeleteEnrollmentServerResponse{EnrollmentTokenID: enrollmentTokenID}, nil
}

func (f *fakeInventory) ListZoneOversubscriptionTargets(_ context.Context, zoneID string) (thunder.ZoneOversubscriptionTargetsResponse, error) {
	if f.targetsErr != nil {
		return thunder.ZoneOversubscriptionTargetsResponse{}, f.targetsErr
	}
	return f.targets[zoneID], nil
}

func testConfig() Config {
	return Config{
		DriverName:                 DefaultDriverName,
		NamePrefix:                 DefaultNamePrefix,
		ZoneLabelKey:               DefaultZoneLabelKey,
		ReconcileInterval:          time.Minute,
		DeviceClassPrefix:          DefaultDeviceClassPrefix,
		ExtendedResourcePrefix:     DefaultExtendedResourcePrefix,
		DefaultHostArtifactProfile: hostartifacts.ProfileDriver,
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

	pools, err := buildDesiredPools(context.Background(), inventory, nil)
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
	op := New(testConfig(), kube, nil, inventory, nil)

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

func TestSyncContinuesPastFailingPool(t *testing.T) {
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("create", "resourceslices", func(action k8stesting.Action) (bool, runtime.Object, error) {
		slice := action.(k8stesting.CreateAction).GetObject().(*resourcev1.ResourceSlice)
		if strings.Contains(slice.Spec.Pool.Name, "aaa-zone") {
			return true, nil, errors.New("apiserver rejected aaa-zone")
		}
		return false, nil, nil
	})
	inventory := &fakeInventory{
		zones: []thunder.Zone{
			{ZoneID: "z1", DisplayName: "aaa-zone"},
			{ZoneID: "z2", DisplayName: "bbb-zone"},
		},
		nodes: map[string][]thunder.Server{
			"z1": {{GPUType: "T4", GPUCount: 1, Status: "active"}},
			"z2": {{GPUType: "A6000", GPUCount: 1, Status: "active"}},
		},
	}
	op := New(testConfig(), kube, nil, inventory, nil)

	err := op.Sync(context.Background())
	if err == nil {
		t.Fatal("Sync error = nil, want joined error containing the aaa-zone failure")
	}
	list, listErr := kube.ResourceV1().ResourceSlices().List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatalf("list ResourceSlices: %v", listErr)
	}
	var foundB bool
	for _, s := range list.Items {
		if strings.Contains(s.Spec.Pool.Name, "bbb-zone") {
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("bbb-zone pool not published; failing aaa-zone pool starved it (slices=%d)", len(list.Items))
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

func TestOversubscribedRoundsDown(t *testing.T) {
	tests := []struct {
		hosts  int64
		target float64
		want   int64
	}{
		{hosts: 4, target: 1, want: 4},
		{hosts: 4, target: 1.5, want: 6},
		{hosts: 3, target: 1.5, want: 4}, // 4.5 rounds down, never up
		{hosts: 10, target: 2.25, want: 22},
		{hosts: 4, target: 0.5, want: 2}, // targets below 1 hold GPUs back
		{hosts: 0, target: 4, want: 0},
		{hosts: 4, target: 0, want: 0},
	}
	for _, test := range tests {
		if got := oversubscribed(test.hosts, test.target); got != test.want {
			t.Fatalf("oversubscribed(%d, %v) = %d, want %d", test.hosts, test.target, got, test.want)
		}
	}
}

func TestOversubscriptionTargetsFallBack(t *testing.T) {
	targets := oversubscriptionTargets{
		byGPUType: map[string]float64{"a6000": 2},
		fallback:  1.5,
	}
	if got := targets.For("A6000"); got != 2 {
		t.Fatalf("For(A6000) = %v, want 2", got)
	}
	if got := targets.For("h100"); got != 1.5 {
		t.Fatalf("For(h100) = %v, want the zone default 1.5", got)
	}

	// An absent or malformed target must not empty a zone.
	empty := oversubscriptionTargets{}
	if got := empty.For("a6000"); got != 1 {
		t.Fatalf("For with no targets = %v, want 1", got)
	}
	zeroed := oversubscriptionTargets{byGPUType: map[string]float64{"a6000": 0}, fallback: 0}
	if got := zeroed.For("a6000"); got != 1 {
		t.Fatalf("For with a zero target = %v, want 1", got)
	}
}

func TestBuildDesiredPoolsAppliesOversubscription(t *testing.T) {
	inventory := &fakeInventory{
		zones: []thunder.Zone{{ZoneID: "zone-1", DisplayName: "us-west-2a"}},
		nodes: map[string][]thunder.Server{
			"zone-1": {{GPUType: "A6000", GPUCount: 4, Status: "active"}},
		},
		targets: map[string]thunder.ZoneOversubscriptionTargetsResponse{
			"zone-1": {
				OversubscriptionTargets: []thunder.ZoneOversubscriptionTarget{
					{GPUType: "A6000", OversubscriptionTarget: 1.5},
				},
			},
		},
	}

	pools, err := buildDesiredPools(context.Background(), inventory, nil)
	if err != nil {
		t.Fatalf("buildDesiredPools: %v", err)
	}
	definition := pools[poolKey{Zone: "us-west-2a", GPUType: "a6000"}]
	if definition.HostCapacity != 4 {
		t.Fatalf("HostCapacity = %d, want 4", definition.HostCapacity)
	}
	if definition.Oversubscription != 1.5 {
		t.Fatalf("Oversubscription = %v, want 1.5", definition.Oversubscription)
	}
	// 4 physical GPUs at 1.5x are published as 6 devices.
	if definition.Capacity != 6 {
		t.Fatalf("Capacity = %d, want 6", definition.Capacity)
	}

	slices := buildResourceSlices(testConfig(), definition, 1)
	if got := len(slices[0].Spec.Devices); got != 6 {
		t.Fatalf("devices = %d, want 6", got)
	}
	if got := slices[0].Labels[oversubscriptionLabelName]; got != "1.5" {
		t.Fatalf("oversubscription label = %q, want 1.5", got)
	}
}

func TestBuildDesiredPoolsSurvivesOversubscriptionErrors(t *testing.T) {
	// Capacity policy must not be able to take inventory down.
	inventory := &fakeInventory{
		zones: []thunder.Zone{{ZoneID: "zone-1", DisplayName: "us-west-2a"}},
		nodes: map[string][]thunder.Server{
			"zone-1": {{GPUType: "A6000", GPUCount: 4, Status: "active"}},
		},
		targetsErr: errors.New("forbidden"),
	}

	pools, err := buildDesiredPools(context.Background(), inventory, nil)
	if err != nil {
		t.Fatalf("buildDesiredPools: %v", err)
	}
	if definition := pools[poolKey{Zone: "us-west-2a", GPUType: "a6000"}]; definition.Capacity != 4 {
		t.Fatalf("Capacity = %d, want 4 (no oversubscription)", definition.Capacity)
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
	if len(class.Spec.Config) != 1 || class.Spec.Config[0].Opaque == nil {
		t.Fatalf("config = %#v", class.Spec.Config)
	}
	profile, err := hostartifacts.Decode(class.Spec.Config[0].Opaque.Parameters)
	if err != nil {
		t.Fatalf("decode class config: %v", err)
	}
	if profile != hostartifacts.ProfileDriver {
		t.Fatalf("class profile = %q, want driver", profile)
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
	op := New(testConfig(), kube, nil, &fakeInventory{}, nil)

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
	op := New(testConfig(), kube, nil, &fakeInventory{}, nil)

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
	op := New(cfg, kube, nil, &fakeInventory{}, nil)

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
