package operator

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

type Operator struct {
	cfg       Config
	kube      kubernetes.Interface
	inventory InventorySource
	logger    *slog.Logger
	cache     map[poolKey]publishedPool
}

type publishedPool struct {
	Definition poolDefinition
	Generation int64
}

func New(cfg Config, kube kubernetes.Interface, inventory InventorySource, logger *slog.Logger) *Operator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Operator{
		cfg:       cfg,
		kube:      kube,
		inventory: inventory,
		logger:    logger,
		cache:     map[poolKey]publishedPool{},
	}
}

func (o *Operator) Run(ctx context.Context) error {
	if err := o.Sync(ctx); err != nil {
		o.logger.Error("initial reconcile failed", "error", err)
	}

	ticker := time.NewTicker(o.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := o.Sync(ctx); err != nil {
				o.logger.Error("reconcile failed", "error", err)
			}
		}
	}
}

func (o *Operator) Sync(ctx context.Context) error {
	existing, err := o.listExistingResourceSlices(ctx)
	if err != nil {
		return err
	}

	desired, err := buildDesiredPools(ctx, o.inventory, o.logger)
	if err != nil {
		return err
	}

	for key, slices := range existing {
		if _, ok := desired[key]; ok {
			continue
		}
		for _, slice := range slices {
			if err := o.deleteSlice(ctx, slice); err != nil {
				return err
			}
		}
		delete(o.cache, key)
		o.logger.Info("deleted stale pool", "zone", key.Zone, "gpuType", key.GPUType, "slices", len(slices))
	}

	for _, key := range sortedPoolKeys(desired) {
		definition := desired[key]
		current := existing[key]
		generation := o.nextGeneration(key, definition, current)
		wanted := buildResourceSlices(o.cfg, definition, generation)

		if err := o.applyPool(ctx, key, current, wanted, definition, generation); err != nil {
			return err
		}
		o.cache[key] = publishedPool{Definition: definition, Generation: generation}
	}

	for key := range o.cache {
		if _, ok := desired[key]; !ok {
			delete(o.cache, key)
		}
	}

	// Device classes come last: a class is only useful once the devices it
	// selects are published.
	return o.syncDeviceClasses(ctx, desired)
}

// applyPool reconciles every shard of one pool. A pool is published as several
// slices when the zone holds more GPUs than fit in one, so shards that are no
// longer needed have to be deleted as the zone shrinks.
func (o *Operator) applyPool(
	ctx context.Context,
	key poolKey,
	current []*resourcev1.ResourceSlice,
	wanted []*resourcev1.ResourceSlice,
	definition poolDefinition,
	generation int64,
) error {
	byName := make(map[string]*resourcev1.ResourceSlice, len(current))
	for _, slice := range current {
		byName[slice.Name] = slice
	}

	for _, slice := range wanted {
		existing, ok := byName[slice.Name]
		delete(byName, slice.Name)

		if !ok {
			if _, err := o.kube.ResourceV1().ResourceSlices().Create(ctx, slice, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create ResourceSlice %s: %w", slice.Name, err)
			}
			o.logger.Info("created ResourceSlice", "name", slice.Name, "generation", generation,
				"devices", len(slice.Spec.Devices), "gpus", definition.Capacity)
			continue
		}
		if samePublishedResourceSlice(existing, slice) {
			continue
		}
		updated := existing.DeepCopy()
		updated.Labels = slice.Labels
		updated.Spec = slice.Spec
		if _, err := o.kube.ResourceV1().ResourceSlices().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update ResourceSlice %s: %w", slice.Name, err)
		}
		o.logger.Info("updated ResourceSlice", "name", slice.Name, "generation", generation,
			"devices", len(slice.Spec.Devices), "gpus", definition.Capacity)
	}

	// Whatever is left belonged to a wider version of this pool.
	for _, stale := range byName {
		if err := o.deleteSlice(ctx, stale); err != nil {
			return err
		}
		o.logger.Info("deleted surplus ResourceSlice", "name", stale.Name,
			"zone", key.Zone, "gpuType", key.GPUType)
	}
	return nil
}

func (o *Operator) deleteSlice(ctx context.Context, slice *resourcev1.ResourceSlice) error {
	err := o.kube.ResourceV1().ResourceSlices().Delete(ctx, slice.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ResourceSlice %s: %w", slice.Name, err)
	}
	return nil
}

func (o *Operator) nextGeneration(key poolKey, definition poolDefinition, current []*resourcev1.ResourceSlice) int64 {
	if len(current) > 0 && currentDefinitionMatches(o.cfg, current, definition) {
		if generation := current[0].Spec.Pool.Generation; generation > 0 {
			return generation
		}
		return 1
	}
	if cached, ok := o.cache[key]; ok {
		if reflect.DeepEqual(cached.Definition, definition) {
			return cached.Generation
		}
		return cached.Generation + 1
	}
	if len(current) == 0 || current[0].Spec.Pool.Generation <= 0 {
		return 1
	}
	return current[0].Spec.Pool.Generation + 1
}

func (o *Operator) listExistingResourceSlices(ctx context.Context) (map[poolKey][]*resourcev1.ResourceSlice, error) {
	selector := labels.Set{
		"app.kubernetes.io/name":      driverAppName,
		"app.kubernetes.io/component": resourceInventoryComponent,
	}.String()
	fieldSelector := fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorDriver, o.cfg.DriverName).String()
	list, err := o.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		FieldSelector: fieldSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("list ResourceSlices: %w", err)
	}

	existing := map[poolKey][]*resourcev1.ResourceSlice{}
	for i := range list.Items {
		item := &list.Items[i]
		key, ok := resourceSliceKey(item)
		if !ok {
			o.logger.Warn("ignoring Thunder ResourceSlice with unrecognized pool", "name", item.Name, "pool", item.Spec.Pool.Name)
			continue
		}
		existing[key] = append(existing[key], item)
	}

	for key, slices := range existing {
		sort.Slice(slices, func(i, j int) bool { return slices[i].Name < slices[j].Name })
		if _, ok := o.cache[key]; ok {
			continue
		}
		definition := definitionFromResourceSlices(slices)
		if definition.Capacity > 0 {
			o.cache[key] = publishedPool{Definition: definition, Generation: slices[0].Spec.Pool.Generation}
		}
	}
	return existing, nil
}

func resourceSliceKey(slice *resourcev1.ResourceSlice) (poolKey, bool) {
	parts := strings.Split(slice.Spec.Pool.Name, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return poolKey{}, false
	}
	return poolKey{Zone: parts[0], GPUType: parts[1]}, true
}

func samePublishedResourceSlice(current, wanted *resourcev1.ResourceSlice) bool {
	return reflect.DeepEqual(current.Labels, wanted.Labels) && reflect.DeepEqual(current.Spec, wanted.Spec)
}

func currentDefinitionMatches(cfg Config, current []*resourcev1.ResourceSlice, definition poolDefinition) bool {
	wanted := buildResourceSlices(Config{
		DriverName:        current[0].Spec.Driver,
		NamePrefix:        DefaultNamePrefix,
		ZoneLabelKey:      cfg.ZoneLabelKey,
		ReconcileInterval: time.Minute,
	}, definition, current[0].Spec.Pool.Generation)
	if len(wanted) != len(current) {
		return false
	}
	for i := range wanted {
		if !reflect.DeepEqual(current[i].Spec, wanted[i].Spec) {
			return false
		}
	}
	return true
}

// definitionFromResourceSlices recovers what a published pool represents. The
// GPU count is the total number of devices across every shard.
func definitionFromResourceSlices(slices []*resourcev1.ResourceSlice) poolDefinition {
	if len(slices) == 0 {
		return poolDefinition{}
	}
	key, ok := resourceSliceKey(slices[0])
	if !ok {
		return poolDefinition{}
	}
	capacity := int64(0)
	for _, slice := range slices {
		capacity += int64(len(slice.Spec.Devices))
	}
	return poolDefinition{
		Zone:           key.Zone,
		GPUType:        key.GPUType,
		HostCapacity:   parseIntLabel(slices[0].Labels[hostCapacityLabelName]),
		ClientCapacity: parseIntLabel(slices[0].Labels[clientCapacityLabelName]),
		Capacity:       capacity,
	}
}

func parseIntLabel(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
