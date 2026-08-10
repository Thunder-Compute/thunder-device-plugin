package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// thunderDomain qualifies every attribute, capacity and label the operator
// publishes.
const thunderDomain = "thundercompute.com"

const (
	resourceInventoryComponent = "resource-inventory"
	driverAppName              = "thunder-dra-driver"

	zoneAttributeName    = thunderDomain + "/zone"
	gpuTypeAttributeName = thunderDomain + "/gpu_type"

	// sharesCapacityName counts how many clients may share one GPU. It is only
	// published when oversubscription is configured.
	sharesCapacityName = thunderDomain + "/shares"

	// Labels recording the inputs that produced a slice, so a later reconcile
	// can compare against them without re-querying Thunder.
	zoneLabelName           = thunderDomain + "/zone"
	gpuTypeLabelName        = thunderDomain + "/gpu_type"
	hostCapacityLabelName   = thunderDomain + "/host-capacity"
	clientCapacityLabelName = thunderDomain + "/client-capacity"
	shardLabelName          = thunderDomain + "/shard"

	// devicesPerSlice mirrors resource.ResourceSliceMaxDevices. A zone with
	// more GPUs than this is published as several slices in one pool.
	devicesPerSlice = 128

	// maxDNSLabel is the length limit for a device or object name.
	maxDNSLabel = 63
)

var invalidDNSLabelRun = regexp.MustCompile(`[^a-z0-9-]+`)

// buildResourceSlices renders a zone's GPUs of one type as ResourceSlices.
//
// Each GPU is published as its own device, so a workload asks for several GPUs
// with a device count rather than a capacity request. That is what lets the
// same pool satisfy the extended resource form (`thundercompute.com/gpu: 2`),
// which the scheduler always translates into a count of devices.
//
// A slice holds at most devicesPerSlice devices, so a large zone is sharded
// across several slices sharing one pool name and generation.
func buildResourceSlices(cfg Config, definition poolDefinition, generation int64) []*resourcev1.ResourceSlice {
	total := definition.Capacity
	if total <= 0 {
		return nil
	}
	shards := int((total + devicesPerSlice - 1) / devicesPerSlice)

	slices := make([]*resourcev1.ResourceSlice, 0, shards)
	for shard := 0; shard < shards; shard++ {
		first := int64(shard) * devicesPerSlice
		last := first + devicesPerSlice
		if last > total {
			last = total
		}

		devices := make([]resourcev1.Device, 0, last-first)
		for index := first; index < last; index++ {
			devices = append(devices, buildDevice(cfg, definition, index))
		}
		slices = append(slices, buildSlice(cfg, definition, generation, shard, shards, devices))
	}
	return slices
}

func buildSlice(cfg Config, definition poolDefinition, generation int64, shard, shards int, devices []resourcev1.Device) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: resourcev1.SchemeGroupVersion.String(),
			Kind:       "ResourceSlice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: resourceSliceName(cfg.NamePrefix, definition.Zone, definition.GPUType, shard),
			Labels: map[string]string{
				"app.kubernetes.io/name":       driverAppName,
				"app.kubernetes.io/component":  resourceInventoryComponent,
				"app.kubernetes.io/managed-by": "thunder-dra-operator",
				zoneLabelName:                  dnsLabel(definition.Zone),
				gpuTypeLabelName:               dnsLabel(definition.GPUType),
				hostCapacityLabelName:          fmt.Sprint(definition.HostCapacity),
				clientCapacityLabelName:        fmt.Sprint(definition.ClientCapacity),
				shardLabelName:                 strconv.Itoa(shard),
			},
		},
		Spec: resourcev1.ResourceSliceSpec{
			Driver: cfg.DriverName,
			Pool: resourcev1.ResourcePool{
				Name:               poolName(definition.Zone, definition.GPUType),
				Generation:         generation,
				ResourceSliceCount: int64(shards),
			},
			NodeSelector: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      cfg.ZoneLabelKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{definition.Zone},
							},
						},
					},
				},
			},
			Devices: devices,
		},
	}
}

// buildDevice renders one GPU. Without oversubscription the device carries no
// capacity at all, which keeps the driver on plain DRA and off the consumable
// capacity feature gate.
func buildDevice(cfg Config, definition poolDefinition, index int64) resourcev1.Device {
	zone := definition.Zone
	gpuType := strings.ToUpper(definition.GPUType)

	device := resourcev1.Device{
		Name: deviceName(definition.GPUType, index),
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			resourcev1.QualifiedName(gpuTypeAttributeName): {StringValue: &gpuType},
			resourcev1.QualifiedName(zoneAttributeName):    {StringValue: &zone},
		},
	}

	if cfg.SharesPerGPU > 1 {
		allowMultipleAllocations := true
		one := apiresource.MustParse("1")
		device.AllowMultipleAllocations = &allowMultipleAllocations
		device.Capacity = map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			resourcev1.QualifiedName(sharesCapacityName): {
				Value: apiresource.MustParse(fmt.Sprint(cfg.SharesPerGPU)),
				// A claim takes one share of each GPU it is allocated; asking
				// for more GPUs means asking for more devices.
				RequestPolicy: &resourcev1.CapacityRequestPolicy{
					Default:     quantityPtr(one),
					ValidValues: []apiresource.Quantity{one},
				},
			},
		}
	}
	return device
}

func resourceSliceName(prefix, zone, gpuType string, shard int) string {
	suffix := "-" + strconv.Itoa(shard)
	return truncateLabel(dnsLabel(prefix+"-"+zone+"-"+gpuType), maxDNSLabel-len(suffix)) + suffix
}

func deviceName(gpuType string, index int64) string {
	suffix := "-" + strconv.FormatInt(index, 10)
	return truncateLabel(dnsLabel(gpuType), maxDNSLabel-len(suffix)) + suffix
}

func poolName(zone, gpuType string) string {
	return dnsLabel(zone) + "/" + dnsLabel(gpuType)
}

func dnsLabel(value string) string {
	label := strings.ToLower(strings.TrimSpace(value))
	label = invalidDNSLabelRun.ReplaceAllString(label, "-")
	label = strings.Trim(label, "-")
	if label == "" {
		return "unknown"
	}
	return truncateLabel(label, maxDNSLabel)
}

// truncateLabel shortens a label to max characters, keeping it unique by
// replacing the tail with a hash of the original.
func truncateLabel(label string, max int) string {
	if len(label) <= max {
		return label
	}
	sum := sha256.Sum256([]byte(label))
	hash := hex.EncodeToString(sum[:])[:10]
	return strings.TrimSuffix(label[:max-len(hash)-1], "-") + "-" + hash
}

func quantityPtr(value apiresource.Quantity) *apiresource.Quantity {
	copied := value.DeepCopy()
	return &copied
}
