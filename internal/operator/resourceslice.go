package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
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
	gpuCountCapacityName = thunderDomain + "/gpu_count"

	// Labels recording the inputs that produced a slice, so a later reconcile
	// can compare against them without re-querying Thunder.
	zoneLabelName           = thunderDomain + "/zone"
	gpuTypeLabelName        = thunderDomain + "/gpu_type"
	hostCapacityLabelName   = thunderDomain + "/host-capacity"
	clientCapacityLabelName = thunderDomain + "/client-capacity"
)

var invalidDNSLabelRun = regexp.MustCompile(`[^a-z0-9-]+`)

func buildResourceSlice(cfg Config, definition poolDefinition, generation int64) *resourcev1.ResourceSlice {
	zoneLabel := dnsLabel(definition.Zone)
	gpuLabel := dnsLabel(definition.GPUType)
	zone := definition.Zone
	gpuType := strings.ToUpper(definition.GPUType)
	allowMultipleAllocations := true
	requestPolicy := capacityRequestPolicy(cfg.ValidGPUCounts, definition.Capacity)

	return &resourcev1.ResourceSlice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: resourcev1.SchemeGroupVersion.String(),
			Kind:       "ResourceSlice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: resourceSliceName(cfg.NamePrefix, definition.Zone, definition.GPUType),
			Labels: map[string]string{
				"app.kubernetes.io/name":       driverAppName,
				"app.kubernetes.io/component":  resourceInventoryComponent,
				"app.kubernetes.io/managed-by": "thunder-dra-operator",
				zoneLabelName:                  zoneLabel,
				gpuTypeLabelName:               gpuLabel,
				hostCapacityLabelName:          fmt.Sprint(definition.HostCapacity),
				clientCapacityLabelName:        fmt.Sprint(definition.ClientCapacity),
			},
		},
		Spec: resourcev1.ResourceSliceSpec{
			Driver: cfg.DriverName,
			Pool: resourcev1.ResourcePool{
				Name:               poolName(definition.Zone, definition.GPUType),
				Generation:         generation,
				ResourceSliceCount: 1,
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
			Devices: []resourcev1.Device{
				{
					Name:                     dnsLabel(definition.GPUType) + "-capacity",
					AllowMultipleAllocations: &allowMultipleAllocations,
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName(gpuTypeAttributeName): {
							StringValue: &gpuType,
						},
						resourcev1.QualifiedName(zoneAttributeName): {
							StringValue: &zone,
						},
					},
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
						resourcev1.QualifiedName(gpuCountCapacityName): {
							Value:         apiresource.MustParse(fmt.Sprint(definition.Capacity)),
							RequestPolicy: requestPolicy,
						},
					},
				},
			},
		},
	}
}

// capacityRequestPolicy builds the request policy for a device's GPU count.
//
// The API server rejects a slice whose validValues contain an option larger
// than the device capacity, and rejects a default that is not itself a valid
// value. A zone can easily hold fewer GPUs than the largest configured count,
// so the configured options are clamped to what the zone can actually serve
// and the default is the smallest surviving option. When nothing survives the
// device is published without a request policy rather than not at all.
func capacityRequestPolicy(validGPUCounts []string, capacity int64) *resourcev1.CapacityRequestPolicy {
	validValues := make([]apiresource.Quantity, 0, len(validGPUCounts))
	smallest := int64(0)
	for _, value := range validGPUCounts {
		count, err := strconv.ParseInt(value, 10, 64)
		if err != nil || count <= 0 || count > capacity {
			continue
		}
		validValues = append(validValues, apiresource.MustParse(value))
		if smallest == 0 || count < smallest {
			smallest = count
		}
	}
	if len(validValues) == 0 {
		return nil
	}
	sort.Slice(validValues, func(i, j int) bool {
		return validValues[i].Cmp(validValues[j]) < 0
	})
	return &resourcev1.CapacityRequestPolicy{
		Default:     quantityPtr(apiresource.MustParse(fmt.Sprint(smallest))),
		ValidValues: validValues,
	}
}

func resourceSliceName(prefix, zone, gpuType string) string {
	base := dnsLabel(prefix + "-" + zone + "-" + gpuType)
	if len(base) <= 63 {
		return base
	}
	sum := sha256.Sum256([]byte(base))
	suffix := hex.EncodeToString(sum[:])[:10]
	return strings.TrimSuffix(base[:52], "-") + "-" + suffix
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
	if len(label) <= 63 {
		return label
	}
	sum := sha256.Sum256([]byte(label))
	suffix := hex.EncodeToString(sum[:])[:10]
	return strings.TrimSuffix(label[:52], "-") + "-" + suffix
}

func quantityPtr(value apiresource.Quantity) *apiresource.Quantity {
	copy := value.DeepCopy()
	return &copy
}
