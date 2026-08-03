package operator

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
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

	desired, err := buildDesiredPools(ctx, o.inventory)
	if err != nil {
		return err
	}

	for key, slice := range existing {
		if _, ok := desired[key]; ok {
			continue
		}
		if err := o.kube.ResourceV1().ResourceSlices().Delete(ctx, slice.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale ResourceSlice %s: %w", slice.Name, err)
		}
		delete(o.cache, key)
		o.logger.Info("deleted stale ResourceSlice", "name", slice.Name, "zone", key.Zone, "gpuType", key.GPUType)
	}

	for _, key := range sortedPoolKeys(desired) {
		definition := desired[key]
		current := existing[key]
		generation := o.nextGeneration(key, definition, current)
		wanted := buildResourceSlice(o.cfg, definition, generation)

		if current == nil {
			if _, err := o.kube.ResourceV1().ResourceSlices().Create(ctx, wanted, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create ResourceSlice %s: %w", wanted.Name, err)
			}
			o.logger.Info("created ResourceSlice", "name", wanted.Name, "generation", generation, "capacity", definition.Capacity)
		} else if !samePublishedResourceSlice(current, wanted) {
			updated := current.DeepCopy()
			updated.Labels = wanted.Labels
			updated.Spec = wanted.Spec
			if _, err := o.kube.ResourceV1().ResourceSlices().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("update ResourceSlice %s: %w", wanted.Name, err)
			}
			o.logger.Info("updated ResourceSlice", "name", wanted.Name, "generation", generation, "capacity", definition.Capacity)
		}

		o.cache[key] = publishedPool{Definition: definition, Generation: generation}
	}

	for key := range o.cache {
		if _, ok := desired[key]; !ok {
			delete(o.cache, key)
		}
	}
	return nil
}

func (o *Operator) nextGeneration(key poolKey, definition poolDefinition, current *resourcev1.ResourceSlice) int64 {
	if current != nil && currentDefinitionMatches(current, definition, o.cfg.ValidGPUCounts, o.cfg.ZoneLabelKey) {
		if current.Spec.Pool.Generation > 0 {
			return current.Spec.Pool.Generation
		}
		return 1
	}
	if cached, ok := o.cache[key]; ok {
		if reflect.DeepEqual(cached.Definition, definition) {
			return cached.Generation
		}
		return cached.Generation + 1
	}
	if current == nil || current.Spec.Pool.Generation <= 0 {
		return 1
	}
	return current.Spec.Pool.Generation + 1
}

func (o *Operator) listExistingResourceSlices(ctx context.Context) (map[poolKey]*resourcev1.ResourceSlice, error) {
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

	existing := map[poolKey]*resourcev1.ResourceSlice{}
	for i := range list.Items {
		item := &list.Items[i]
		key, ok := resourceSliceKey(item)
		if !ok {
			o.logger.Warn("ignoring Thunder ResourceSlice with unrecognized pool", "name", item.Name, "pool", item.Spec.Pool.Name)
			continue
		}
		existing[key] = item
		if _, ok := o.cache[key]; !ok {
			definition := definitionFromResourceSlice(item)
			if definition.Capacity > 0 {
				o.cache[key] = publishedPool{Definition: definition, Generation: item.Spec.Pool.Generation}
			}
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

func currentDefinitionMatches(current *resourcev1.ResourceSlice, definition poolDefinition, validCounts []string, zoneLabelKey string) bool {
	wanted := buildResourceSlice(Config{
		DriverName:        current.Spec.Driver,
		NamePrefix:        DefaultNamePrefix,
		ZoneLabelKey:      zoneLabelKey,
		ValidGPUCounts:    validCounts,
		ReconcileInterval: time.Minute,
	}, definition, current.Spec.Pool.Generation)
	return reflect.DeepEqual(current.Spec, wanted.Spec)
}

func definitionFromResourceSlice(slice *resourcev1.ResourceSlice) poolDefinition {
	key, ok := resourceSliceKey(slice)
	if !ok || len(slice.Spec.Devices) == 0 {
		return poolDefinition{}
	}
	capacity := int64(0)
	if deviceCapacity, ok := slice.Spec.Devices[0].Capacity[resourcev1.QualifiedName(gpuCountCapacityName)]; ok {
		capacity = deviceCapacity.Value.Value()
	}
	return poolDefinition{
		Zone:           key.Zone,
		GPUType:        key.GPUType,
		HostCapacity:   parseIntLabel(slice.Labels["vgpu.thundercompute.com/host-capacity"]),
		ClientCapacity: parseIntLabel(slice.Labels["vgpu.thundercompute.com/client-capacity"]),
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
