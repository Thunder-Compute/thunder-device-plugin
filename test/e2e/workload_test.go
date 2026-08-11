//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// gpuWorkload is one pod holding one claim, and everything needed to clean it
// up again.
type gpuWorkload struct {
	name     string
	kind     gpuKind
	count    int
	template *resourcev1.ResourceClaimTemplate
	pod      *corev1.Pod
}

// newGPUWorkload creates a ResourceClaimTemplate and a pod that holds a claim
// generated from it. The image is a stock one with no Thunder client, which is
// the property the driver has to make work.
func newGPUWorkload(ctx context.Context, name string, kind gpuKind, count int) (*gpuWorkload, error) {
	template := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: *flagNamespace, Labels: e2eLabels(name)},
		Spec: resourcev1.ResourceClaimTemplateSpec{
			Spec: resourcev1.ResourceClaimSpec{
				Devices: resourcev1.DeviceClaim{
					Requests: []resourcev1.DeviceRequest{{
						Name: "gpu",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: kind.DeviceClassName,
							AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
							Count:           int64(count),
						},
					}},
				},
			},
		},
	}
	if _, err := kubeClient.ResourceV1().ResourceClaimTemplates(*flagNamespace).
		Create(ctx, template, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create ResourceClaimTemplate %s: %w", name, err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: *flagNamespace, Labels: e2eLabels(name)},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Only nodes serving Thunder GPUs run the DRA plugin.
			NodeSelector: map[string]string{*flagDriver + "/node": "true"},
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      "gpu",
				ResourceClaimTemplateName: ptr(name),
			}},
			Containers: []corev1.Container{{
				Name:  "workload",
				Image: *flagImage,
				// exec, so the container answers SIGTERM instead of sitting out
				// the termination grace period on every delete.
				Command: []string{"/bin/sh", "-c", "exec sleep 86400"},
				Resources: corev1.ResourceRequirements{
					Claims: []corev1.ResourceClaim{{Name: "gpu", Request: "gpu"}},
				},
			}},
		},
	}
	if _, err := kubeClient.CoreV1().Pods(*flagNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create pod %s: %w", name, err)
	}
	return &gpuWorkload{name: name, kind: kind, count: count, template: template, pod: pod}, nil
}

func e2eLabels(name string) map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "thunder-e2e", "thunder-e2e/workload": name}
}

func ptr[T any](v T) *T { return &v }

// waitRunning blocks until the pod is Running, or reports why it never was.
func (w *gpuWorkload) waitRunning(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pod, err := kubeClient.CoreV1().Pods(*flagNamespace).Get(ctx, w.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read pod %s: %w", w.name, err)
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("pod %s failed: %s", w.name, podTrouble(pod))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %s still %s after %s: %s", w.name, pod.Status.Phase, timeout, podTrouble(pod))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// podTrouble summarises why a pod is not running, preferring the container
// message the runtime produced over the scheduler's.
func podTrouble(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if state := status.State.Waiting; state != nil && state.Message != "" {
			return fmt.Sprintf("%s: %s", state.Reason, state.Message)
		}
		if state := status.State.Terminated; state != nil && state.Message != "" {
			return fmt.Sprintf("%s: %s", state.Reason, state.Message)
		}
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Status != corev1.ConditionTrue && condition.Message != "" {
			return fmt.Sprintf("%s: %s", condition.Reason, condition.Message)
		}
	}
	return string(pod.Status.Phase)
}

// isPending reports whether the pod is waiting for resources it cannot get.
func (w *gpuWorkload) isPending(ctx context.Context) (bool, string, error) {
	pod, err := kubeClient.CoreV1().Pods(*flagNamespace).Get(ctx, w.name, metav1.GetOptions{})
	if err != nil {
		return false, "", err
	}
	return pod.Status.Phase == corev1.PodPending, podTrouble(pod), nil
}

// exec runs a command in the workload container and returns its combined
// output.
func (w *gpuWorkload) exec(ctx context.Context, command ...string) (string, error) {
	request := kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").Name(w.name).Namespace(*flagNamespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "workload",
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", request.URL())
	if err != nil {
		return "", fmt.Errorf("build exec for %s: %w", w.name, err)
	}
	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	output := stdout.String() + stderr.String()
	if err != nil {
		return output, fmt.Errorf("exec %v in %s: %w: %s", command, w.name, err, strings.TrimSpace(output))
	}
	return output, nil
}

// waitGPUReady runs nvidia-smi until it answers. A container can start before
// the node's thunderd has picked up the new client, and until it does the
// library reports the runtime as unavailable.
func (w *gpuWorkload) waitGPUReady(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		output, err := w.exec(ctx, "nvidia-smi")
		if err == nil && strings.Contains(output, "NVIDIA-SMI") && !strings.Contains(output, "Failed to initialize NVML") {
			return output, nil
		}
		last = output
		if err != nil && last == "" {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("no GPU in %s after %s: %s", w.name, timeout, strings.TrimSpace(last))
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// countGPUs counts the GPU rows nvidia-smi printed.
func countGPUs(nvidiaSMI string) int {
	count := 0
	for _, line := range strings.Split(nvidiaSMI, "\n") {
		// Device rows look like "|   0  NVIDIA RTX A6000  On  | ...".
		if strings.HasPrefix(strings.TrimSpace(line), "|") && strings.Contains(line, "NVIDIA") &&
			!strings.Contains(line, "NVIDIA-SMI") {
			count++
		}
	}
	return count
}

// claimName reports the name of the claim generated for this pod, which the
// driver derives its ThunderClient and CDI device from.
func (w *gpuWorkload) claimName(ctx context.Context) (string, error) {
	pod, err := kubeClient.CoreV1().Pods(*flagNamespace).Get(ctx, w.name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, status := range pod.Status.ResourceClaimStatuses {
		if status.ResourceClaimName != nil {
			return *status.ResourceClaimName, nil
		}
	}
	return "", fmt.Errorf("pod %s has no generated claim yet", w.name)
}

// remove deletes the pod and its template and waits for both to go away, so a
// caller can assert on what the driver released.
func (w *gpuWorkload) remove(ctx context.Context) error {
	background := metav1.DeletePropagationBackground
	options := metav1.DeleteOptions{PropagationPolicy: &background}

	if err := kubeClient.CoreV1().Pods(*flagNamespace).Delete(ctx, w.name, options); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", w.name, err)
	}
	if err := kubeClient.ResourceV1().ResourceClaimTemplates(*flagNamespace).
		Delete(ctx, w.name, options); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ResourceClaimTemplate %s: %w", w.name, err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := kubeClient.CoreV1().Pods(*flagNamespace).Get(ctx, w.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %s did not go away within 2m", w.name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// cleanupWorkload removes a workload at the end of a test, honouring -e2e.keep
// so a failure can be inspected.
func cleanupWorkload(t *testing.T, w *gpuWorkload) {
	t.Helper()
	if w == nil {
		return
	}
	if *flagKeep && t.Failed() {
		t.Logf("-e2e.keep set: leaving %s behind", w.name)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := w.remove(ctx); err != nil {
		t.Errorf("cleanup %s: %v", w.name, err)
	}
}

// waitQuiescent waits until nothing this suite created is still holding a
// Thunder client, which is what "everything was released" means.
func waitQuiescent(t *testing.T, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, "every Thunder client to be revoked", func(ctx context.Context) error {
		clients, err := countThunderClients(ctx)
		if err != nil {
			return err
		}
		if clients != 0 {
			return fmt.Errorf("%d ThunderClient(s) still exist", clients)
		}
		claims, err := kubeClient.ResourceV1().ResourceClaims(*flagNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		if len(claims.Items) != 0 {
			return fmt.Errorf("%d ResourceClaim(s) still exist", len(claims.Items))
		}
		return nil
	})
}
