package operator

import (
	"context"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/thunderclient"
)

func thunderClientObject(name, claimNamespace, claimName, claimUID, tokenID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "thundercompute.com/v1alpha1",
		"kind":       "ThunderClient",
		"metadata": map[string]any{
			"name":       name,
			"namespace":  "thunder-system",
			"finalizers": []any{thunderclient.Finalizer},
		},
		"spec": map[string]any{
			"claimUID":       claimUID,
			"claimNamespace": claimNamespace,
			"claimName":      claimName,
		},
		"status": map[string]any{"enrollmentTokenID": tokenID},
	}}
}

func newReaper(t *testing.T, clients []*unstructured.Unstructured, claims []runtime.Object, grace time.Duration) (*Operator, *fakeInventory, *dynamicfake.FakeDynamicClient) {
	t.Helper()

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		thunderclient.GVR.GroupVersion().WithKind("ThunderClientList"),
		&unstructured.UnstructuredList{},
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{thunderclient.GVR: "ThunderClientList"})

	// Seed through the resource interface rather than the tracker: the fake
	// guesses the resource from the kind, and ThunderClient does not pluralize
	// to "clients". The real dynamic client is always told the GVR outright.
	for _, client := range clients {
		if _, err := dyn.Resource(thunderclient.GVR).Namespace(client.GetNamespace()).
			Create(context.Background(), client, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed %s: %v", client.GetName(), err)
		}
	}

	thunder := &fakeInventory{}
	cfg := testConfig()
	cfg.OrphanGracePeriod = grace
	op := New(cfg, fake.NewSimpleClientset(claims...), dyn, thunder, nil)
	return op, thunder, dyn
}

func remaining(t *testing.T, dyn *dynamicfake.FakeDynamicClient) []string {
	t.Helper()
	list, err := dyn.Resource(thunderclient.GVR).Namespace(metav1.NamespaceAll).
		List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names
}

func liveClaim(namespace, name, uid string) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(uid)},
	}
}

func TestReapLeavesClientsWhoseClaimStillExists(t *testing.T) {
	client := thunderClientObject("claim-a", "default", "pod-gpu", "uid-1", "token-1")
	op, thunder, dyn := newReaper(t, []*unstructured.Unstructured{client},
		[]runtime.Object{liveClaim("default", "pod-gpu", "uid-1")}, 0)

	if err := op.reapOrphanedClients(context.Background()); err != nil {
		t.Fatalf("reapOrphanedClients: %v", err)
	}
	if len(thunder.revoked) != 0 {
		t.Fatalf("revoked %v for a live claim", thunder.revoked)
	}
	if names := remaining(t, dyn); len(names) != 1 {
		t.Fatalf("remaining = %v, want the client kept", names)
	}
}

func TestReapWaitsOutTheGracePeriod(t *testing.T) {
	client := thunderClientObject("claim-a", "default", "pod-gpu", "uid-1", "token-1")
	op, thunder, dyn := newReaper(t, []*unstructured.Unstructured{client}, nil, 5*time.Minute)

	now := time.Now()
	op.clock = func() time.Time { return now }

	// A claim is deleted slightly before the kubelet finishes unpreparing, so
	// the first sighting must never reap.
	if err := op.reapOrphanedClients(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(thunder.revoked) != 0 || len(remaining(t, dyn)) != 1 {
		t.Fatalf("reaped on first sighting: revoked=%v remaining=%v", thunder.revoked, remaining(t, dyn))
	}

	// Still inside the grace period.
	now = now.Add(4 * time.Minute)
	if err := op.reapOrphanedClients(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(thunder.revoked) != 0 {
		t.Fatalf("reaped inside the grace period: %v", thunder.revoked)
	}

	// Past it.
	now = now.Add(2 * time.Minute)
	if err := op.reapOrphanedClients(context.Background()); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if len(thunder.revoked) != 1 || thunder.revoked[0] != "token-1" {
		t.Fatalf("revoked = %v, want [token-1]", thunder.revoked)
	}
	if names := remaining(t, dyn); len(names) != 0 {
		t.Fatalf("remaining = %v, want the client removed", names)
	}
}

func TestReapTreatsARecreatedClaimAsGone(t *testing.T) {
	// Same name, different claim: the enrollment belongs to the old one.
	client := thunderClientObject("claim-a", "default", "pod-gpu", "uid-old", "token-1")
	op, thunder, _ := newReaper(t, []*unstructured.Unstructured{client},
		[]runtime.Object{liveClaim("default", "pod-gpu", "uid-new")}, 0)

	for i := 0; i < 2; i++ {
		if err := op.reapOrphanedClients(context.Background()); err != nil {
			t.Fatalf("reapOrphanedClients: %v", err)
		}
	}
	if len(thunder.revoked) != 1 {
		t.Fatalf("revoked = %v, want the stale enrollment revoked", thunder.revoked)
	}
}

func TestReapKeepsTheClientWhenRevokeFails(t *testing.T) {
	// Dropping the resource after a failed revoke would destroy the only record
	// of the leak, so the resource has to survive to be retried.
	client := thunderClientObject("claim-a", "default", "pod-gpu", "uid-1", "token-1")
	op, thunder, dyn := newReaper(t, []*unstructured.Unstructured{client}, nil, 0)
	thunder.revokeErr = context.DeadlineExceeded

	for i := 0; i < 2; i++ {
		if err := op.reapOrphanedClients(context.Background()); err == nil && i == 1 {
			t.Fatal("reapOrphanedClients succeeded despite a failed revoke")
		}
	}
	if names := remaining(t, dyn); len(names) != 1 {
		t.Fatalf("remaining = %v, want the client kept for retry", names)
	}
}

func TestReapIgnoresClientsWithNoClaimReference(t *testing.T) {
	client := thunderClientObject("claim-a", "", "", "", "token-1")
	op, thunder, dyn := newReaper(t, []*unstructured.Unstructured{client}, nil, 0)

	for i := 0; i < 2; i++ {
		if err := op.reapOrphanedClients(context.Background()); err != nil {
			t.Fatalf("reapOrphanedClients: %v", err)
		}
	}
	if len(thunder.revoked) != 0 || len(remaining(t, dyn)) != 1 {
		t.Fatalf("acted on a client with nothing to correlate against")
	}
}
