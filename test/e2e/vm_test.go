//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	virtualMachineGVR = schema.GroupVersionResource{
		Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines",
	}
	virtualMachineInstanceGVR = schema.GroupVersionResource{
		Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances",
	}
)

// TestKubeVirtVM checks the VM path: a VM declares a claim, and Thunder gives
// its guest what it needs to become a client.
//
// The guest is not driven to completion here. Booting a real distribution and
// running the installer takes minutes and exercises the Thunder installer more
// than this driver. What this test owns is everything up to that point: the
// claim is prepared, the setup Secret is written with both keys, the VM mounts
// it, and teardown releases the enrolment.
func TestKubeVirtVM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	if !kubeVirtInstalled(ctx) {
		t.Skip("KubeVirt is not installed on this cluster")
	}

	kind := inventory.largest()
	name := uniqueName("vm")
	claimName := name + "-claim"
	secretName := claimName + "-thunder-setup"

	// A VM mounts the Secret by name, so the claim needs a name chosen up
	// front rather than one generated from a template.
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: *flagNamespace, Labels: e2eLabels(name)},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{
					Name: "gpu",
					Exactly: &resourcev1.ExactDeviceRequest{
						DeviceClassName: kind.DeviceClassName,
						AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
						Count:           1,
					},
				}},
			},
		},
	}
	if _, err := kubeClient.ResourceV1().ResourceClaims(*flagNamespace).
		Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ResourceClaim: %v", err)
	}

	vm := virtualMachine(name, claimName, secretName)
	if _, err := dynClient.Resource(virtualMachineGVR).Namespace(*flagNamespace).
		Create(ctx, vm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create VirtualMachine: %v", err)
	}
	defer cleanupVM(t, name, claimName)

	t.Run("the VM runs", func(t *testing.T) {
		eventually(t, *flagReadyTimeout, "the VM to reach Running", func(ctx context.Context) error {
			vmi, err := dynClient.Resource(virtualMachineInstanceGVR).Namespace(*flagNamespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("no VirtualMachineInstance yet: %w", err)
			}
			phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
			if phase != "Running" {
				return fmt.Errorf("VM is %s", phase)
			}
			return nil
		})
	})

	t.Run("the guest setup Secret is written", func(t *testing.T) {
		var secret *corev1.Secret
		eventually(t, 2*time.Minute, "the guest setup Secret", func(ctx context.Context) error {
			var err error
			secret, err = kubeClient.CoreV1().Secrets(*flagNamespace).Get(ctx, secretName, metav1.GetOptions{})
			return err
		})
		// Both halves have to be there: the token alone cannot install
		// anything, and the script alone has nothing to enrol with.
		for _, key := range []string{"enrollment-token", "install-thunder-client.sh"} {
			if len(secret.Data[key]) == 0 {
				t.Errorf("guest setup Secret has no %s", key)
			}
		}
	})

	t.Run("the claim is enrolled", func(t *testing.T) {
		eventually(t, 2*time.Minute, "a ThunderClient for the VM claim", func(ctx context.Context) error {
			found, err := thunderClientForClaim(ctx, claimName)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("no ThunderClient records claim %s", claimName)
			}
			return nil
		})
	})

	t.Run("deleting the VM releases everything", func(t *testing.T) {
		cleanupVM(t, name, claimName)
		eventually(t, *flagQuiesce, "the VM's enrolment to be revoked", func(ctx context.Context) error {
			found, err := thunderClientForClaim(ctx, claimName)
			if err != nil {
				return err
			}
			if found {
				return fmt.Errorf("ThunderClient for %s still exists", claimName)
			}
			if _, err := kubeClient.CoreV1().Secrets(*flagNamespace).
				Get(ctx, secretName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return fmt.Errorf("guest setup Secret still exists")
			}
			return nil
		})
	})
}

// virtualMachine builds a VM that holds a claim and mounts the setup Secret.
//
// It boots from a container disk rather than a DataVolume: the test is about
// the claim and the Secret, and a container disk keeps it off CDI and off a
// multi-hundred-megabyte image import. No GPU device is declared — a Thunder
// GPU is reached over the network by the guest, not passed through.
func virtualMachine(name, claimName, secretName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":      name,
			"namespace": *flagNamespace,
			"labels":    toAnyMap(e2eLabels(name)),
		},
		"spec": map[string]any{
			"runStrategy": "Always",
			"template": map[string]any{
				"metadata": map[string]any{"labels": toAnyMap(e2eLabels(name))},
				"spec": map[string]any{
					"nodeSelector": map[string]any{*flagDriver + "/node": "true"},
					"resourceClaims": []any{map[string]any{
						"name":              "gpu",
						"resourceClaimName": claimName,
					}},
					"domain": map[string]any{
						"resources": map[string]any{
							"requests": map[string]any{"memory": "512Mi"},
						},
						"devices": map[string]any{
							"disks": []any{map[string]any{
								"name": "boot",
								"disk": map[string]any{"bus": "virtio"},
							}},
							"filesystems": []any{map[string]any{
								"name":     "thunder-setup",
								"virtiofs": map[string]any{},
							}},
						},
					},
					"volumes": []any{
						map[string]any{
							"name":          "boot",
							"containerDisk": map[string]any{"image": *flagVMContainerDisk},
						},
						map[string]any{
							"name": "thunder-setup",
							"secret": map[string]any{
								"secretName": secretName,
								// The Secret does not exist until the claim is
								// prepared, which happens as the VM starts.
								"optional": true,
							},
						},
					},
				},
			},
		},
	}}
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func kubeVirtInstalled(ctx context.Context) bool {
	_, err := dynClient.Resource(virtualMachineGVR).Namespace(*flagNamespace).
		List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

func cleanupVM(t *testing.T, name, claimName string) {
	t.Helper()
	if *flagKeep && t.Failed() {
		t.Logf("-e2e.keep set: leaving VM %s behind", name)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := dynClient.Resource(virtualMachineGVR).Namespace(*flagNamespace).
		Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete VirtualMachine %s: %v", name, err)
	}
	if err := kubeClient.ResourceV1().ResourceClaims(*flagNamespace).
		Delete(ctx, claimName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete ResourceClaim %s: %v", claimName, err)
	}
}
