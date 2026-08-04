package operator

import (
	"context"
	"fmt"
	"sort"
	"strings"

	thunder "thunder-device-plugin/pkg/thunder-sdk"
)

type InventorySource interface {
	ListZones(ctx context.Context) ([]thunder.Zone, error)
	ListNodes(ctx context.Context, zoneID string) ([]thunder.Node, error)
	ListClients(ctx context.Context, zoneID string) ([]thunder.ClientNode, error)
}

type poolKey struct {
	Zone    string
	GPUType string
}

type poolDefinition struct {
	Zone           string
	GPUType        string
	HostCapacity   int64
	ClientCapacity int64
	Capacity       int64
}

func buildDesiredPools(ctx context.Context, inventory InventorySource) (map[poolKey]poolDefinition, error) {
	zones, err := inventory.ListZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("list thunder zones: %w", err)
	}

	desired := map[poolKey]poolDefinition{}
	for _, zone := range zones {
		zoneID := strings.TrimSpace(zone.ZoneID)
		zoneName := strings.TrimSpace(zone.DisplayName)
		if zoneName == "" {
			zoneName = zoneID
		}
		if zoneID == "" {
			zoneID = zoneName
		}
		if zoneName == "" || zoneID == "" {
			continue
		}

		hostCounts := map[string]int64{}
		nodes, err := inventory.ListNodes(ctx, zoneID)
		if err != nil {
			return nil, fmt.Errorf("list thunder nodes in zone %q: %w", zoneID, err)
		}
		for _, node := range nodes {
			if !nodeHealthy(node.Status) {
				continue
			}
			addGPUCount(hostCounts, node.GPUType, int64(node.GPUCount))
		}

		clientCounts := map[string]int64{}
		clients, err := inventory.ListClients(ctx, zoneID)
		if err != nil {
			return nil, fmt.Errorf("list thunder clients in zone %q: %w", zoneID, err)
		}
		for _, client := range clients {
			addGPUCount(clientCounts, client.GPUType, int64(client.GPUCount))
		}

		gpuTypes := map[string]struct{}{}
		for gpuType := range hostCounts {
			gpuTypes[gpuType] = struct{}{}
		}
		for gpuType := range clientCounts {
			gpuTypes[gpuType] = struct{}{}
		}

		for gpuType := range gpuTypes {
			hostCapacity := hostCounts[gpuType]
			clientCapacity := clientCounts[gpuType]
			capacity := hostCapacity
			if clientCapacity > capacity {
				capacity = clientCapacity
			}
			if capacity <= 0 {
				continue
			}
			key := poolKey{Zone: zoneName, GPUType: gpuType}
			definition := desired[key]
			definition.Zone = zoneName
			definition.GPUType = gpuType
			definition.HostCapacity += hostCapacity
			definition.ClientCapacity += clientCapacity
			definition.Capacity = definition.HostCapacity
			if definition.ClientCapacity > definition.Capacity {
				definition.Capacity = definition.ClientCapacity
			}
			desired[key] = definition
		}
	}
	return desired, nil
}

func nodeHealthy(status string) bool {
	status = strings.TrimSpace(strings.ToLower(status))
	return status == "" || status == "active" || status == "online" || status == "ready" || status == "healthy"
}

func addGPUCount(counts map[string]int64, gpuType string, count int64) {
	normalized := normalizeGPUType(gpuType)
	if normalized == "" || count <= 0 {
		return
	}
	counts[normalized] += count
}

func normalizeGPUType(gpuType string) string {
	return strings.ToLower(strings.TrimSpace(gpuType))
}

func sortedPoolKeys(pools map[poolKey]poolDefinition) []poolKey {
	keys := make([]poolKey, 0, len(pools))
	for key := range pools {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Zone == keys[j].Zone {
			return keys[i].GPUType < keys[j].GPUType
		}
		return keys[i].Zone < keys[j].Zone
	})
	return keys
}
