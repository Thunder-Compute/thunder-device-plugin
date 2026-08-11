package operator

import (
	"context"
	"fmt"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const deviceClassComponent = "device-class"

// buildDeviceClass renders the DeviceClass that backs one GPU type.
//
// A generic extended resource cannot express "all of these GPUs must be the
// same model": DeviceClass carries selectors, which are evaluated per device,
// and has no equivalent of a claim's matchAttribute constraint. So a request
// for `thundercompute.com/gpu: 4` in a zone holding two models can be satisfied
// with a mix of them. Publishing one class per GPU type makes the model part of
// the resource name, which is the only way to pin it.
func buildDeviceClass(cfg Config, gpuType string) *resourcev1.DeviceClass {
	// The operator writes the attribute upper-cased, so the selector has to
	// compare against the same form.
	attributeValue := strings.ToUpper(gpuType)

	extendedResource := extendedResourceName(cfg.ExtendedResourcePrefix, gpuType)

	return &resourcev1.DeviceClass{
		TypeMeta: metav1.TypeMeta{
			APIVersion: resourcev1.SchemeGroupVersion.String(),
			Kind:       "DeviceClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: deviceClassName(cfg.DeviceClassPrefix, gpuType),
			Labels: map[string]string{
				"app.kubernetes.io/name":       driverAppName,
				"app.kubernetes.io/component":  deviceClassComponent,
				"app.kubernetes.io/managed-by": "thunder-dra-operator",
				gpuTypeLabelName:               dnsLabel(gpuType),
			},
		},
		Spec: resourcev1.DeviceClassSpec{
			ExtendedResourceName: &extendedResource,
			Selectors: []resourcev1.DeviceSelector{
				{
					CEL: &resourcev1.CELDeviceSelector{
						Expression: fmt.Sprintf(
							"device.driver == %q && device.attributes[%q][%q] == %q",
							cfg.DriverName, thunderDomain, "gpu_type", attributeValue),
					},
				},
			},
		},
	}
}

func deviceClassName(prefix, gpuType string) string {
	return dnsLabel(prefix + gpuType)
}

// extendedResourceName is the name workloads put in resources.limits. It is a
// fully qualified resource name, so the GPU type becomes the resource's own
// name rather than something the scheduler is free to choose.
func extendedResourceName(prefix, gpuType string) string {
	return prefix + dnsLabel(gpuType)
}

// syncDeviceClasses reconciles one DeviceClass per GPU type currently in
// inventory, and removes the classes of GPU types that went away.
func (o *Operator) syncDeviceClasses(ctx context.Context, desired map[poolKey]poolDefinition) error {
	existing, err := o.listManagedDeviceClasses(ctx)
	if err != nil {
		return err
	}

	wanted := map[string]*resourcev1.DeviceClass{}
	if o.cfg.ExtendedResourcePrefix != "" {
		for key := range desired {
			class := buildDeviceClass(o.cfg, key.GPUType)
			wanted[class.Name] = class
		}
	}

	for name, class := range wanted {
		current, ok := existing[name]
		delete(existing, name)

		if !ok {
			if _, err := o.kube.ResourceV1().DeviceClasses().Create(ctx, class, metav1.CreateOptions{}); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return fmt.Errorf("create DeviceClass %s: %w", name, err)
			}
			o.logger.Info("created DeviceClass", "name", name,
				"extendedResourceName", derefString(class.Spec.ExtendedResourceName))
			continue
		}
		if sameDeviceClass(current, class) {
			continue
		}
		updated := current.DeepCopy()
		updated.Labels = class.Labels
		updated.Spec = class.Spec
		if _, err := o.kube.ResourceV1().DeviceClasses().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update DeviceClass %s: %w", name, err)
		}
		o.logger.Info("updated DeviceClass", "name", name,
			"extendedResourceName", derefString(class.Spec.ExtendedResourceName))
	}

	for name := range existing {
		err := o.kube.ResourceV1().DeviceClasses().Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale DeviceClass %s: %w", name, err)
		}
		o.logger.Info("deleted stale DeviceClass", "name", name)
	}
	return nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (o *Operator) listManagedDeviceClasses(ctx context.Context) (map[string]*resourcev1.DeviceClass, error) {
	selector := labels.Set{
		"app.kubernetes.io/name":       driverAppName,
		"app.kubernetes.io/component":  deviceClassComponent,
		"app.kubernetes.io/managed-by": "thunder-dra-operator",
	}.String()
	list, err := o.kube.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list DeviceClasses: %w", err)
	}
	classes := make(map[string]*resourcev1.DeviceClass, len(list.Items))
	for i := range list.Items {
		classes[list.Items[i].Name] = &list.Items[i]
	}
	return classes, nil
}

func sameDeviceClass(current, wanted *resourcev1.DeviceClass) bool {
	if derefString(current.Spec.ExtendedResourceName) != derefString(wanted.Spec.ExtendedResourceName) {
		return false
	}
	if len(current.Spec.Selectors) != len(wanted.Spec.Selectors) {
		return false
	}
	for i := range wanted.Spec.Selectors {
		a, b := current.Spec.Selectors[i].CEL, wanted.Spec.Selectors[i].CEL
		if a == nil || b == nil || a.Expression != b.Expression {
			return false
		}
	}
	for key, value := range wanted.Labels {
		if current.Labels[key] != value {
			return false
		}
	}
	return true
}
