//go:build e2e

// Package e2e drives a real cluster that already has the chart installed.
//
// These tests allocate real GPUs, enrol real Thunder clients and delete them
// again. They are never run by `go test ./...`: the e2e build tag keeps them
// out, and `make test-e2e` is the way in.
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	flagNamespace = flag.String("e2e.namespace", "thunder-e2e", "namespace the tests create workloads in")
	flagDriver    = flag.String("e2e.driver", "thundercompute.com", "DRA driver name to test")
	flagImage     = flag.String("e2e.image", "ubuntu:22.04", "workload image; it deliberately ships no Thunder client")
	flagSystemNS  = flag.String("e2e.system-namespace", "thunder-system", "namespace the chart is installed in")

	flagReadyTimeout = flag.Duration("e2e.ready-timeout", 4*time.Minute, "how long a pod may take to reach Running")
	flagGPUTimeout   = flag.Duration("e2e.gpu-timeout", 90*time.Second, "how long a container may take before its GPU answers")
	flagQuiesce      = flag.Duration("e2e.quiesce-timeout", 3*time.Minute, "how long teardown may take to release everything")

	flagStressDuration = flag.Duration("e2e.stress-duration", 5*time.Minute, "how long the stress test churns for")
	flagStressWorkers  = flag.Int("e2e.stress-workers", 6, "how many workers churn concurrently")

	flagVMContainerDisk = flag.String("e2e.vm-container-disk",
		"quay.io/kubevirt/cirros-container-disk-demo:latest",
		"boot disk for the KubeVirt test; a container disk keeps the test off CDI and off a 600MB import")

	flagKeep = flag.Bool("e2e.keep", false, "leave workloads behind on failure for debugging")
)

// thunderClientGVR is the per-claim record the daemon writes. It is the
// cheapest proof that a claim was prepared and later released.
var thunderClientGVR = schema.GroupVersionResource{
	Group:    "thundercompute.com",
	Version:  "v1alpha1",
	Resource: "clients",
}

var (
	kubeClient *kubernetes.Clientset
	dynClient  dynamic.Interface
	restConfig *rest.Config
	inventory  gpuInventory
)

func TestMain(m *testing.M) {
	flag.Parse()

	if err := connect(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := ensureNamespace(ctx, *flagNamespace); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	var err error
	// Discovering rather than hardcoding is what lets the same suite run
	// against any cluster. A cluster with no usable GPU is a failure, not a
	// skip: the whole point of these tests is the GPU path.
	if inventory, err = discoverGPUs(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("e2e: driver %s, usable GPUs: %s\n", *flagDriver, inventory)

	os.Exit(m.Run())
}

func connect() error {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return fmt.Errorf("no kubeconfig: %w (set KUBECONFIG to the cluster under test)", err)
	}
	// The stress test runs many pods at once and the default client throttles
	// hard enough to look like cluster latency.
	cfg.QPS, cfg.Burst = 100, 200
	restConfig = cfg

	if kubeClient, err = kubernetes.NewForConfig(cfg); err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}
	if dynClient, err = dynamic.NewForConfig(cfg); err != nil {
		return fmt.Errorf("build dynamic client: %w", err)
	}
	return nil
}

func ensureNamespace(ctx context.Context, name string) error {
	_, err := kubeClient.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read namespace %s: %w", name, err)
	}
	_, err = kubeClient.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", name, err)
	}
	return nil
}

// gpuKind is one GPU model the cluster can actually schedule.
type gpuKind struct {
	GPUType         string // as Thunder reports it, e.g. "A6000"
	DeviceClassName string // e.g. "thunder-gpu-a6000"
	Capacity        int    // devices published in pools a node can reach
}

type gpuInventory struct {
	kinds []gpuKind
}

func (i gpuInventory) String() string {
	if len(i.kinds) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(i.kinds))
	for _, k := range i.kinds {
		parts = append(parts, fmt.Sprintf("%s x%d (%s)", k.GPUType, k.Capacity, k.DeviceClassName))
	}
	return strings.Join(parts, ", ")
}

// largest returns the kind with the most capacity, which gives multi-GPU tests
// the best chance of fitting.
func (i gpuInventory) largest() gpuKind {
	best := i.kinds[0]
	for _, k := range i.kinds[1:] {
		if k.Capacity > best.Capacity {
			best = k
		}
	}
	return best
}

// discoverGPUs reads what this cluster can actually run.
//
// A ResourceSlice is only useful if some schedulable node satisfies its node
// selector: a zone pool whose GPUs no node can reach publishes devices that
// never schedule. Counting those would make the suite request capacity that
// cannot exist and blame the driver for the resulting Pending pod.
func discoverGPUs(ctx context.Context) (gpuInventory, error) {
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return gpuInventory{}, fmt.Errorf("list nodes: %w", err)
	}

	classes, err := kubeClient.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return gpuInventory{}, fmt.Errorf("list DeviceClasses: %w", err)
	}
	classByGPUType := map[string]string{}
	for _, class := range classes.Items {
		if gpuType := class.Labels[*flagDriver+"/gpu_type"]; gpuType != "" {
			classByGPUType[strings.ToLower(gpuType)] = class.Name
		}
	}

	slices, err := kubeClient.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return gpuInventory{}, fmt.Errorf("list ResourceSlices: %w", err)
	}

	capacity := map[string]int{}
	gpuTypeNames := map[string]string{}
	for _, slice := range slices.Items {
		if slice.Spec.Driver != *flagDriver {
			continue
		}
		if !anyNodeMatches(nodes.Items, slice.Spec.NodeSelector) {
			continue
		}
		for _, device := range slice.Spec.Devices {
			attr, ok := device.Attributes[resourcev1.QualifiedName(*flagDriver+"/gpu_type")]
			if !ok || attr.StringValue == nil {
				continue
			}
			gpuType := *attr.StringValue
			key := strings.ToLower(gpuType)
			capacity[key]++
			gpuTypeNames[key] = gpuType
		}
	}

	var kinds []gpuKind
	for key, count := range capacity {
		class, ok := classByGPUType[key]
		if !ok {
			// Devices with no DeviceClass cannot be requested by name.
			continue
		}
		kinds = append(kinds, gpuKind{GPUType: gpuTypeNames[key], DeviceClassName: class, Capacity: count})
	}
	if len(kinds) == 0 {
		return gpuInventory{}, fmt.Errorf(
			"no schedulable %s GPUs in this cluster: %d DeviceClasses and %d ResourceSlices exist, "+
				"but none publish devices a node can reach. Check that a node carries the zone label "+
				"of a pool with capacity",
			*flagDriver, len(classes.Items), len(slices.Items))
	}
	return gpuInventory{kinds: kinds}, nil
}

// anyNodeMatches reports whether any node satisfies the slice's node selector.
// A nil selector means the devices are reachable from anywhere.
func anyNodeMatches(nodes []corev1.Node, selector *corev1.NodeSelector) bool {
	if selector == nil || len(selector.NodeSelectorTerms) == 0 {
		return len(nodes) > 0
	}
	for _, node := range nodes {
		if node.Spec.Unschedulable {
			continue
		}
		for _, term := range selector.NodeSelectorTerms {
			if matchesTerm(node.Labels, term) {
				return true
			}
		}
	}
	return false
}

func matchesTerm(labels map[string]string, term corev1.NodeSelectorTerm) bool {
	for _, expr := range term.MatchExpressions {
		value, present := labels[expr.Key]
		switch expr.Operator {
		case corev1.NodeSelectorOpIn:
			if !present || !contains(expr.Values, value) {
				return false
			}
		case corev1.NodeSelectorOpNotIn:
			if present && contains(expr.Values, value) {
				return false
			}
		case corev1.NodeSelectorOpExists:
			if !present {
				return false
			}
		case corev1.NodeSelectorOpDoesNotExist:
			if present {
				return false
			}
		default:
			// Numeric comparisons are not used by this driver; treat an
			// unknown operator as unmatched rather than silently passing.
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// driverHealth is a snapshot of the chart's own pods, so a test can prove the
// driver came through a run in the same shape it started in.
type driverHealth struct {
	pods     int
	ready    int
	restarts int32
}

func snapshotDriver(ctx context.Context) (driverHealth, error) {
	pods, err := kubeClient.CoreV1().Pods(*flagSystemNS).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=thunder-device-plugin",
	})
	if err != nil {
		return driverHealth{}, fmt.Errorf("list driver pods: %w", err)
	}
	health := driverHealth{pods: len(pods.Items)}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		allReady := len(pod.Status.ContainerStatuses) > 0
		for _, status := range pod.Status.ContainerStatuses {
			if !status.Ready {
				allReady = false
			}
			health.restarts += status.RestartCount
		}
		if allReady {
			health.ready++
		}
	}
	return health, nil
}

func (h driverHealth) String() string {
	return fmt.Sprintf("%d/%d pods ready, %d restarts", h.ready, h.pods, h.restarts)
}

// countThunderClients reports how many per-claim client records exist, across
// every namespace. Zero is the only correct answer once nothing is running.
func countThunderClients(ctx context.Context) (int, error) {
	list, err := dynClient.Resource(thunderClientGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("list ThunderClients: %w", err)
	}
	return len(list.Items), nil
}

// eventually retries until the condition holds or the deadline passes. The
// last failure is reported, so a timeout says what was actually wrong.
func eventually(t *testing.T, timeout time.Duration, what string, condition func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var last error
	for {
		if last = condition(ctx); last == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
		case <-time.After(2 * time.Second):
		}
	}
}
