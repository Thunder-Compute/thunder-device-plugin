package daemon

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func TestPrepareResourceClaimsMintsTokenCreatesClientAndReturnsCDI(t *testing.T) {
	ctx := context.Background()
	claimUID := types.UID("11111111-1111-1111-1111-111111111111")
	shareID := types.UID("22222222-2222-2222-2222-222222222222")
	claim := testClaim(claimUID, &shareID, 4)
	kube := fake.NewSimpleClientset(testSlice())
	tokens := &fakeTokenIssuer{}
	clients := newMemoryClientStore()
	cdi := &memoryCDIStore{}
	driver := &Driver{
		DriverName: DefaultDriverName,
		NodeName:   "node-a",
		Kube:       kube,
		Tokens:     tokens,
		Clients:    clients,
		CDI:        cdi,
		Guest:      &memoryGuestStore{},
	}

	result, err := driver.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("PrepareResourceClaims returned batch error: %v", err)
	}
	prepared := result[claimUID]
	if prepared.Err != nil {
		t.Fatalf("PrepareResourceClaims claim error: %v", prepared.Err)
	}
	// One entry per allocated GPU, all pointing at the one Thunder client.
	if len(prepared.Devices) != 4 {
		t.Fatalf("prepared devices = %d, want 4", len(prepared.Devices))
	}
	for i, device := range prepared.Devices {
		wantName := "a6000-" + strconv.Itoa(i)
		if device.PoolName != "us-west-2a/a6000" || device.DeviceName != wantName {
			t.Fatalf("device %d = %#v, want device %s", i, device, wantName)
		}
		if len(device.CDIDeviceIDs) != 1 || device.CDIDeviceIDs[0] != "thundercompute.com/gpu=claim-11111111-1111-1111-1111-111111111111" {
			t.Fatalf("device %d CDI IDs = %#v", i, device.CDIDeviceIDs)
		}
	}
	if tokens.minted != 1 {
		t.Fatalf("minted = %d, want 1", tokens.minted)
	}
	if tokens.lastAllocation.Zone != "us-west-2a" || tokens.lastAllocation.GPUType != "A6000" || tokens.lastAllocation.GPUCount != 4 {
		t.Fatalf("mint allocation = %#v", tokens.lastAllocation)
	}
	client, err := clients.Get(ctx, claimUID)
	if err != nil {
		t.Fatalf("ThunderClient missing: %v", err)
	}
	if client.EnrollmentTokenID != "token-id-1" || client.CDIName == "" {
		t.Fatalf("ThunderClient = %#v", client)
	}
	if client.GuestNamespace != "default" || client.GuestSecret != "claim-a-thunder-setup" {
		t.Fatalf("ThunderClient guest artifacts = %#v", client)
	}
	if client.NodeName != "node-a" || client.Consumer.Namespace != "default" || client.Consumer.Name != "pod-a" {
		t.Fatalf("ThunderClient scheduling data = %#v", client)
	}
}

func TestPrepareResourceClaimsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	claimUID := types.UID("11111111-1111-1111-1111-111111111111")
	claim := testClaim(claimUID, nil, 2)
	driver := &Driver{
		DriverName: DefaultDriverName,
		NodeName:   "node-a",
		Kube:       fake.NewSimpleClientset(testSlice()),
		Tokens:     &fakeTokenIssuer{},
		Clients:    newMemoryClientStore(),
		CDI:        &memoryCDIStore{},
		Guest:      &memoryGuestStore{},
	}

	first, err := driver.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{claim})
	if err != nil || first[claimUID].Err != nil {
		t.Fatalf("first prepare error: batch=%v claim=%v", err, first[claimUID].Err)
	}
	second, err := driver.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{claim})
	if err != nil || second[claimUID].Err != nil {
		t.Fatalf("second prepare error: batch=%v claim=%v", err, second[claimUID].Err)
	}
	tokens := driver.Tokens.(*fakeTokenIssuer)
	if tokens.minted != 1 {
		t.Fatalf("minted = %d, want 1", tokens.minted)
	}
}

func TestUnprepareResourceClaimsRevokesTokenAndDeletesClient(t *testing.T) {
	ctx := context.Background()
	claimUID := types.UID("11111111-1111-1111-1111-111111111111")
	clients := newMemoryClientStore()
	if err := clients.Upsert(ctx, ThunderClient{
		ClaimUID:          claimUID,
		ClaimNamespace:    "default",
		ClaimName:         "claim-a",
		EnrollmentTokenID: "token-id-1",
		CDIName:           "thundercompute.com/gpu=claim-11111111-1111-1111-1111-111111111111",
		GuestNamespace:    "default",
		GuestSecret:       "claim-a-thunder-setup",
	}); err != nil {
		t.Fatal(err)
	}
	tokens := &fakeTokenIssuer{}
	cdi := &memoryCDIStore{created: map[string]bool{"thundercompute.com/gpu=claim-11111111-1111-1111-1111-111111111111": true}}
	guest := &memoryGuestStore{created: map[string]bool{"default/claim-a-thunder-setup": true}}
	driver := &Driver{Tokens: tokens, Clients: clients, CDI: cdi, Guest: guest}

	result, err := driver.UnprepareResourceClaims(ctx, []kubeletplugin.NamespacedObject{{NamespacedName: types.NamespacedName{Namespace: "default", Name: "claim-a"}, UID: claimUID}})
	if err != nil {
		t.Fatalf("UnprepareResourceClaims returned batch error: %v", err)
	}
	if result[claimUID] != nil {
		t.Fatalf("UnprepareResourceClaims claim error: %v", result[claimUID])
	}
	if len(tokens.revoked) != 1 || tokens.revoked[0] != "token-id-1" {
		t.Fatalf("revoked = %#v", tokens.revoked)
	}
	if _, err := clients.Get(ctx, claimUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ThunderClient get error = %v, want ErrNotFound", err)
	}
	if cdi.created["thundercompute.com/gpu=claim-11111111-1111-1111-1111-111111111111"] {
		t.Fatalf("CDI device still exists")
	}
	if guest.created["default/claim-a-thunder-setup"] {
		t.Fatalf("guest artifacts still exist: %#v", guest.created)
	}
}

// testClaim allocates gpuCount devices, which is how the scheduler represents a
// multi-GPU claim now that the operator publishes one device per GPU.
func testClaim(uid types.UID, shareID *types.UID, gpuCount int64) *resourcev1.ResourceClaim {
	results := make([]resourcev1.DeviceRequestAllocationResult, 0, gpuCount)
	for i := int64(0); i < gpuCount; i++ {
		results = append(results, resourcev1.DeviceRequestAllocationResult{
			Request: "gpu",
			Driver:  DefaultDriverName,
			Pool:    "us-west-2a/a6000",
			Device:  "a6000-" + strconv.FormatInt(i, 10),
			ShareID: shareID,
		})
	}
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "claim-a", UID: uid},
		Status: resourcev1.ResourceClaimStatus{
			ReservedFor: []resourcev1.ResourceClaimConsumerReference{
				{Resource: "pods", Name: "pod-a", UID: types.UID("33333333-3333-3333-3333-333333333333")},
			},
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{Results: results},
			},
		},
	}
}

func testSlice() *resourcev1.ResourceSlice {
	gpuType := "A6000"
	zone := "us-west-2a"
	devices := make([]resourcev1.Device, 0, 4)
	for i := 0; i < 4; i++ {
		devices = append(devices, resourcev1.Device{
			Name: "a6000-" + strconv.Itoa(i),
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(GPUTypeAttributeName): {StringValue: &gpuType},
				resourcev1.QualifiedName(ZoneAttributeName):    {StringValue: &zone},
			},
		})
	}
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "thunder-us-west-2a-a6000-0"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:  DefaultDriverName,
			Pool:    resourcev1.ResourcePool{Name: "us-west-2a/a6000", Generation: 1, ResourceSliceCount: 1},
			Devices: devices,
		},
	}
}

type fakeTokenIssuer struct {
	minted         int
	lastAllocation Allocation
	revoked        []string
	revokedClients []string
}

func (f *fakeTokenIssuer) Mint(ctx context.Context, allocation Allocation) (string, string, time.Time, error) {
	f.minted++
	f.lastAllocation = allocation
	return "token-id-1", "token-value-1", time.Time{}, nil
}

func (f *fakeTokenIssuer) Revoke(ctx context.Context, tokenID string) error {
	f.revoked = append(f.revoked, tokenID)
	return nil
}

func (f *fakeTokenIssuer) RevokeClient(ctx context.Context, clientID string) error {
	f.revokedClients = append(f.revokedClients, clientID)
	return nil
}

type memoryClientStore struct {
	clients map[types.UID]*ThunderClient
}

func newMemoryClientStore() *memoryClientStore {
	return &memoryClientStore{clients: map[types.UID]*ThunderClient{}}
}

func (s *memoryClientStore) Get(ctx context.Context, claimUID types.UID) (*ThunderClient, error) {
	client, ok := s.clients[claimUID]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *client
	return &copy, nil
}

func (s *memoryClientStore) Upsert(ctx context.Context, client ThunderClient) error {
	s.clients[client.ClaimUID] = &client
	return nil
}

func (s *memoryClientStore) Delete(ctx context.Context, claimUID types.UID) error {
	if _, ok := s.clients[claimUID]; !ok {
		return ErrNotFound
	}
	delete(s.clients, claimUID)
	return nil
}

type memoryCDIStore struct {
	created  map[string]bool
	err      error
	clientID string
}

func (s *memoryCDIStore) Create(ctx context.Context, allocation Allocation, token string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if s.created == nil {
		s.created = map[string]bool{}
	}
	name := DefaultCDIKind + "=" + ThunderClientName(allocation.ClaimUID)
	s.created[name] = true
	return name, nil
}

func (s *memoryCDIStore) StagedClientID(qualifiedName string) string {
	return s.clientID
}

func (s *memoryCDIStore) Remove(ctx context.Context, qualifiedName string) error {
	if s.err != nil {
		return s.err
	}
	if s.created != nil {
		delete(s.created, qualifiedName)
	}
	return nil
}

type memoryGuestStore struct {
	created map[string]bool
	err     error
}

func (s *memoryGuestStore) Create(ctx context.Context, allocation Allocation, token string, installCommand string) (GuestArtifacts, error) {
	if s.err != nil {
		return GuestArtifacts{}, s.err
	}
	if s.created == nil {
		s.created = map[string]bool{}
	}
	artifacts := GuestArtifacts{
		Namespace:  allocation.ClaimNamespace,
		SecretName: ThunderGuestSetupSecretName(allocation.ClaimName),
	}
	s.created[artifacts.Namespace+"/"+artifacts.SecretName] = true
	return artifacts, nil
}

func (s *memoryGuestStore) Remove(ctx context.Context, artifacts GuestArtifacts) error {
	if s.err != nil {
		return s.err
	}
	if s.created != nil {
		delete(s.created, artifacts.Namespace+"/"+artifacts.SecretName)
	}
	return nil
}
