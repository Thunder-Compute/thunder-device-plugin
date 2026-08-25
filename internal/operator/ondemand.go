package operator

import (
	"context"
	"fmt"
	"strings"
)

// SyncPool performs a blocking central-inventory refresh for one allocated
// pool. It is used by the node plugin before preparing a newly scheduled claim:
// if out-of-cluster demand has reduced capacity, the ResourceSlices are shrunk
// before the claim is allowed to start.
func (o *Operator) SyncPool(ctx context.Context, zone, gpuType string) error {
	existing, err := o.listExistingResourceSlices(ctx)
	if err != nil {
		return err
	}
	desired, err := buildDesiredPools(ctx, o.thunder, o.logger)
	if err != nil {
		return err
	}

	key := poolKey{Zone: strings.TrimSpace(zone), GPUType: normalizeGPUType(gpuType)}
	current := existing[key]
	definition, found := desired[key]
	if !found {
		for _, slice := range current {
			if err := o.deleteSlice(ctx, slice); err != nil {
				return fmt.Errorf("delete unavailable pool %s/%s: %w", key.Zone, key.GPUType, err)
			}
		}
		delete(o.cache, key)
		return nil
	}

	generation := o.nextGeneration(key, definition, current)
	wanted := buildResourceSlices(o.cfg, definition, generation)
	if err := o.applyPool(ctx, key, current, wanted, definition, generation); err != nil {
		return fmt.Errorf("apply refreshed pool %s/%s: %w", key.Zone, key.GPUType, err)
	}
	o.cache[key] = publishedPool{Definition: definition, Generation: generation}
	return nil
}
