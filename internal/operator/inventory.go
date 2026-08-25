package operator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

// ThunderAPI is the slice of the Thunder API the operator uses. Everything here
// is a read except UnenrollClient, which is only called to clean up an
// enrollment whose ResourceClaim is already gone.
type ThunderAPI interface {
	ListZones(ctx context.Context) ([]thunder.Zone, error)
	ListServers(ctx context.Context, zoneID string) ([]thunder.Server, error)
	ListClients(ctx context.Context, zoneID string) ([]thunder.RegisteredClient, error)
	ListZoneOversubscriptionTargets(ctx context.Context, zoneID string) (thunder.ZoneOversubscriptionTargetsResponse, error)
	UnenrollClient(ctx context.Context, enrollmentTokenID string) (thunder.DeleteEnrollmentServerResponse, error)
}

type poolKey struct {
	Zone    string
	GPUType string
}

type poolDefinition struct {
	Zone    string
	GPUType string
	// HostCapacity is the number of physical GPUs Thunder reports.
	HostCapacity int64
	// ClientCapacity is the number of GPUs currently committed to clients.
	ClientCapacity int64
	// Oversubscription is the zone's target for this GPU type, read from the
	// Thunder API. 1 means one claim per physical GPU; 2 means twice as many
	// GPUs are offered as exist.
	Oversubscription float64
	// Capacity is how many GPUs are published, after oversubscription.
	Capacity int64
}

func buildDesiredPools(ctx context.Context, inventory ThunderAPI, logger *slog.Logger) (map[poolKey]poolDefinition, error) {
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
		servers, err := inventory.ListServers(ctx, zoneID)
		if err != nil {
			return nil, fmt.Errorf("list thunder servers in zone %q: %w", zoneID, err)
		}
		for _, server := range servers {
			// Cordon is independent of reported health: a healthy cordoned host
			// must not contribute capacity for new claims. Live clients below
			// remain the floor, so removing host capacity never drains them.
			if !nodeHealthy(server.Status) || server.Configurations.Cordoned {
				continue
			}
			addGPUCount(hostCounts, server.GPUType, int64(server.GPUCount))
		}

		clientCounts := map[string]int64{}
		clients, err := inventory.ListClients(ctx, zoneID)
		if err != nil {
			return nil, fmt.Errorf("list thunder clients in zone %q: %w", zoneID, err)
		}
		for _, client := range clients {
			// A decommissioned client is a revoked enrollment, not live
			// demand. Counting it would hold the zone's published capacity up
			// forever, because clientCapacity is a floor: every pod that ever
			// ran would keep its GPU reserved long after it exited.
			if client.DecommissionedAt != nil {
				continue
			}
			addGPUCount(clientCounts, client.GPUType, int64(client.GPUCount))
		}

		targets := zoneOversubscription(ctx, inventory, zoneID, logger)

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
			oversubscription := targets.For(gpuType)

			capacity := oversubscribed(hostCapacity, oversubscription)
			// Committed clients are never dropped from inventory, so a zone
			// whose hosts went quiet still represents its live allocations.
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
			definition.Oversubscription = oversubscription
			definition.Capacity += capacity
			desired[key] = definition
		}
	}
	return desired, nil
}

// oversubscriptionTargets resolves a zone's target per GPU type.
type oversubscriptionTargets struct {
	byGPUType map[string]float64
	fallback  float64
}

// For returns the target for a GPU type, defaulting to the zone default and
// then to 1. A non-positive target is treated as 1 rather than as "publish
// nothing": an unset or malformed value must not silently empty a zone.
func (t oversubscriptionTargets) For(gpuType string) float64 {
	if target, ok := t.byGPUType[normalizeGPUType(gpuType)]; ok && target > 0 {
		return target
	}
	if t.fallback > 0 {
		return t.fallback
	}
	return 1
}

// zoneOversubscription reads the zone's targets from Thunder. The targets are
// advisory capacity policy, so a failure here degrades to no oversubscription
// rather than failing the whole reconcile.
func zoneOversubscription(ctx context.Context, inventory ThunderAPI, zoneID string, logger *slog.Logger) oversubscriptionTargets {
	response, err := inventory.ListZoneOversubscriptionTargets(ctx, zoneID)
	if err != nil {
		if logger != nil {
			logger.Warn("could not read oversubscription targets; assuming 1x",
				"zone", zoneID, "error", err)
		}
		return oversubscriptionTargets{fallback: 1}
	}

	targets := oversubscriptionTargets{
		byGPUType: make(map[string]float64, len(response.OversubscriptionTargets)),
		fallback:  response.DefaultOversubscriptionTarget,
	}
	for _, target := range response.OversubscriptionTargets {
		if gpuType := normalizeGPUType(target.GPUType); gpuType != "" {
			targets.byGPUType[gpuType] = target.OversubscriptionTarget
		}
	}
	return targets
}

// isGone reports whether an error means Thunder no longer has the object, in
// which case the desired state has already been reached.
func isGone(err error) bool {
	if err == nil {
		return false
	}
	if thunder.IsNotFound(err) {
		return true
	}
	var apiErr *thunder.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 410
}

// oversubscribed applies a fractional target to a physical GPU count. It rounds
// down, so a target is never exceeded.
func oversubscribed(hostCapacity int64, target float64) int64 {
	if hostCapacity <= 0 || target <= 0 {
		return 0
	}
	return int64(math.Floor(float64(hostCapacity) * target))
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
