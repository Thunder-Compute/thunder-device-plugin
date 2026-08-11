package operator

import (
	"context"
	"testing"
	"time"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

// A revoked client keeps appearing in ListClients with DecommissionedAt set.
// Counting it would pin the zone's published capacity to the high-water mark of
// every pod that ever ran, because client capacity is a floor.
func TestDecommissionedClientsDoNotHoldCapacity(t *testing.T) {
	revokedAt := time.Now().UTC()
	inventory := &fakeInventory{
		zones: []thunder.Zone{{ZoneID: "zone-1", DisplayName: "us-west-2a"}},
		nodes: map[string][]thunder.Server{
			"zone-1": {{ServerID: "s1", GPUType: "A6000", GPUCount: 2, Status: "active"}},
		},
		clients: map[string][]thunder.RegisteredClient{
			"zone-1": {
				{ClientID: "live", GPUType: "A6000", GPUCount: 1},
				{ClientID: "revoked-1", GPUType: "A6000", GPUCount: 1, DecommissionedAt: &revokedAt},
				{ClientID: "revoked-2", GPUType: "A6000", GPUCount: 1, DecommissionedAt: &revokedAt},
			},
		},
	}

	pools, err := buildDesiredPools(context.Background(), inventory, nil)
	if err != nil {
		t.Fatalf("buildDesiredPools: %v", err)
	}

	var definition poolDefinition
	for key, value := range pools {
		if key.Zone == "us-west-2a" {
			definition = value
		}
	}
	// 2 hosts x 1.0, and only the one live client, so the hosts decide.
	if definition.Capacity != 2 {
		t.Fatalf("Capacity = %d, want 2", definition.Capacity)
	}
	if definition.ClientCapacity != 1 {
		t.Fatalf("ClientCapacity = %d, want 1 (revoked clients excluded)", definition.ClientCapacity)
	}
}
