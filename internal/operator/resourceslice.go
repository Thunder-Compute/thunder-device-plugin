package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	resourceInventoryComponent = "resource-inventory"
	driverAppName              = "thunder-dra-driver"
	zoneAttributeName          = "vgpu.thundercompute.com/zone"
	gpuTypeAttributeName       = "vgpu.thundercompute.com/gpu-type"
	gpuCountCapacityName       = "vgpu.thundercompute.com/gpu-count"
)

var invalidDNSLabelRun = regexp.MustCompile(`[^a-z0-9-]+`)

func buildResourceSlice(cfg Config, definition poolDefinition, generation int64) *resourcev1.ResourceSlice {
	zoneLabel := dnsLabel(definition.Zone)
	gpuLabel := dnsLabel(definition.GPUType)
	zone := definition.Zone
	gpuType := strings.ToUpper(definition.GPUType)
	allowMultipleAllocations := true
	defaultGPUCount := apiresource.MustParse("1")
	validValues := make([]apiresource.Quantity, 0, len(cfg.ValidGPUCounts))
	for _, value := range cfg.ValidGPUCounts {
		validValues = append(validValues, apiresource.MustParse(value))
	}

	return &resourcev1.ResourceSlice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: resourcev1.SchemeGroupVersion.String(),
			Kind:       "ResourceSlice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: resourceSliceName(cfg.NamePrefix, definition.Zone, definition.GPUType),
			Labels: map[string]string{
				"app.kubernetes.io/name":                  driverAppName,
				"app.kubernetes.io/component":             resourceInventoryComponent,
				"app.kubernetes.io/managed-by":            "thunder-dra-operator",
				"vgpu.thundercompute.com/zone":            zoneLabel,
				"vgpu.thundercompute.com/gpu-type":        gpuLabel,
				"vgpu.thundercompute.com/host-capacity":   fmt.Sprint(definition.HostCapacity),
				"vgpu.thundercompute.com/client-capacity": fmt.Sprint(definition.ClientCapacity),
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
							Value: apiresource.MustParse(fmt.Sprint(definition.Capacity)),
							RequestPolicy: &resourcev1.CapacityRequestPolicy{
								Default:     quantityPtr(defaultGPUCount),
								ValidValues: validValues,
							},
						},
					},
				},
			},
		},
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
