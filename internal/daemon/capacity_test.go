package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeCapacityRefresher struct {
	calls   int
	err     error
	refresh func(context.Context, Allocation) error
}

func (f *fakeCapacityRefresher) Refresh(ctx context.Context, allocation Allocation) error {
	f.calls++
	if f.refresh != nil {
		return f.refresh(ctx, allocation)
	}
	return f.err
}

func TestPrepareFailsClosedWhenCapacityRefreshFails(t *testing.T) {
	uid := types.UID("11111111-1111-1111-1111-111111111111")
	refresher := &fakeCapacityRefresher{err: errors.New("central unavailable")}
	tokens := &fakeTokenIssuer{}
	driver := testPrepareDriver(tokens, refresher)

	result, err := driver.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{testClaim(uid, nil, 1)})
	if err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if result[uid].Err == nil || !strings.Contains(result[uid].Err.Error(), "central unavailable") {
		t.Fatalf("claim error = %v", result[uid].Err)
	}
	if tokens.minted != 0 {
		t.Fatalf("minted = %d, want 0", tokens.minted)
	}
}

func TestPrepareFailsWhenRefreshRemovesAllocatedDevice(t *testing.T) {
	uid := types.UID("11111111-1111-1111-1111-111111111111")
	tokens := &fakeTokenIssuer{}
	driver := testPrepareDriver(tokens, nil)
	refresher := &fakeCapacityRefresher{refresh: func(ctx context.Context, allocation Allocation) error {
		slice, err := driver.Kube.ResourceV1().ResourceSlices().Get(ctx, "thunder-us-west-2a-a6000-0", metav1.GetOptions{})
		if err != nil {
			return err
		}
		slice.Spec.Devices = nil
		_, err = driver.Kube.ResourceV1().ResourceSlices().Update(ctx, slice, metav1.UpdateOptions{})
		return err
	}}
	driver.Capacity = refresher

	result, err := driver.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{testClaim(uid, nil, 1)})
	if err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if result[uid].Err == nil || !strings.Contains(result[uid].Err.Error(), "unavailable after refreshing") {
		t.Fatalf("claim error = %v", result[uid].Err)
	}
	if tokens.minted != 0 {
		t.Fatalf("minted = %d, want 0", tokens.minted)
	}
}

func TestPrepareRefreshesCapacityOnlyForANewClaim(t *testing.T) {
	uid := types.UID("11111111-1111-1111-1111-111111111111")
	refresher := &fakeCapacityRefresher{}
	driver := testPrepareDriver(&fakeTokenIssuer{}, refresher)
	claim := testClaim(uid, nil, 1)
	for attempt := 0; attempt < 2; attempt++ {
		result, err := driver.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
		if err != nil || result[uid].Err != nil {
			t.Fatalf("prepare %d: batch=%v claim=%v", attempt+1, err, result[uid].Err)
		}
	}
	if refresher.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresher.calls)
	}
}

func testPrepareDriver(tokens *fakeTokenIssuer, refresher CapacityRefresher) *Driver {
	return &Driver{
		DriverName: DefaultDriverName,
		NodeName:   "node-a",
		Kube:       fake.NewSimpleClientset(testSlice()),
		Tokens:     tokens,
		Clients:    newMemoryClientStore(),
		CDI:        &memoryCDIStore{},
		Guest:      &memoryGuestStore{},
		Capacity:   refresher,
	}
}
