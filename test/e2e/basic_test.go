//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestGPUPodIsUsable is the base case: a stock image with no Thunder client
// gets a working GPU, and the driver leaves nothing behind afterwards.
func TestGPUPodIsUsable(t *testing.T) {
	kind := inventory.largest()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	workload, err := newGPUWorkload(ctx, uniqueName("basic"), kind, 1)
	if err != nil {
		t.Fatalf("create workload: %v", err)
	}
	defer cleanupWorkload(t, workload)

	if err := workload.waitRunning(ctx, *flagReadyTimeout); err != nil {
		t.Fatalf("%v", err)
	}

	t.Run("the GPU answers", func(t *testing.T) {
		output, err := workload.waitGPUReady(ctx, *flagGPUTimeout)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if got := countGPUs(output); got != 1 {
			t.Errorf("nvidia-smi reported %d GPUs, want 1:\n%s", got, output)
		}
		if !strings.Contains(output, kind.GPUType) {
			t.Errorf("nvidia-smi does not mention %s:\n%s", kind.GPUType, output)
		}
	})

	t.Run("the client was staged into the image", func(t *testing.T) {
		// The image ships no Thunder client, so anything here arrived from the
		// CDI hook.
		for _, path := range []string{"/etc/thunder/libthunder.so", "/etc/thunder/config.json"} {
			if _, err := workload.exec(ctx, "test", "-s", path); err != nil {
				t.Errorf("%s is missing from the container: %v", path, err)
			}
		}
		config, err := workload.exec(ctx, "cat", "/etc/thunder/config.json")
		if err != nil {
			t.Fatalf("read config.json: %v", err)
		}
		for _, want := range []string{`"clientId"`, `"authToken"`, `"gpuCount": 1`} {
			if !strings.Contains(config, want) {
				t.Errorf("config.json is missing %s", want)
			}
		}
	})

	t.Run("the container was told what it holds", func(t *testing.T) {
		environment, err := workload.exec(ctx, "sh", "-c", "env")
		if err != nil {
			t.Fatalf("read env: %v", err)
		}
		for _, want := range []string{
			"THUNDER_GPU_TYPE=" + kind.GPUType,
			"THUNDER_GPU_COUNT=1",
			"LD_PRELOAD=/etc/thunder/libthunder.so",
		} {
			if !strings.Contains(environment, want) {
				t.Errorf("container env is missing %s", want)
			}
		}
	})

	t.Run("the claim is recorded", func(t *testing.T) {
		claim, err := workload.claimName(ctx)
		if err != nil {
			t.Fatalf("%v", err)
		}
		found, err := thunderClientForClaim(ctx, claim)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !found {
			t.Errorf("no ThunderClient records claim %s", claim)
		}
	})

	t.Run("deleting the pod releases everything", func(t *testing.T) {
		if err := workload.remove(ctx); err != nil {
			t.Fatalf("%v", err)
		}
		waitQuiescent(t, *flagQuiesce)
	})
}

// TestMultiGPUPod checks that a claim for several GPUs produces several GPUs,
// which is a different code path from one: the driver counts devices rather
// than assuming a single allocation.
func TestMultiGPUPod(t *testing.T) {
	kind := inventory.largest()
	if kind.Capacity < 2 {
		t.Skipf("cluster publishes %d %s GPU(s); need at least 2", kind.Capacity, kind.GPUType)
	}
	want := 2

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	workload, err := newGPUWorkload(ctx, uniqueName("multi"), kind, want)
	if err != nil {
		t.Fatalf("create workload: %v", err)
	}
	defer cleanupWorkload(t, workload)

	if err := workload.waitRunning(ctx, *flagReadyTimeout); err != nil {
		t.Fatalf("%v", err)
	}
	output, err := workload.waitGPUReady(ctx, *flagGPUTimeout)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got := countGPUs(output); got != want {
		t.Errorf("nvidia-smi reported %d GPUs, want %d:\n%s", got, want, output)
	}

	environment, err := workload.exec(ctx, "sh", "-c", "env")
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !strings.Contains(environment, "THUNDER_GPU_COUNT="+strconv.Itoa(want)) {
		t.Errorf("container env does not report %d GPUs", want)
	}
}

// TestOverCapacityRequestIsRejected checks that asking for more than the zone
// publishes fails in the scheduler and costs nothing: no device reserved, no
// client enrolled. An impossible request must not strand capacity.
func TestOverCapacityRequestIsRejected(t *testing.T) {
	kind := inventory.largest()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	before, err := countThunderClients(ctx)
	if err != nil {
		t.Fatalf("%v", err)
	}

	workload, err := newGPUWorkload(ctx, uniqueName("toobig"), kind, kind.Capacity+1)
	if err != nil {
		t.Fatalf("create workload: %v", err)
	}
	defer cleanupWorkload(t, workload)

	// Give the scheduler long enough to have tried and failed.
	time.Sleep(30 * time.Second)

	pending, reason, err := workload.isPending(ctx)
	if err != nil {
		t.Fatalf("read pod: %v", err)
	}
	if !pending {
		t.Fatalf("pod asking for %d %s GPUs is not Pending; the cluster publishes %d",
			kind.Capacity+1, kind.GPUType, kind.Capacity)
	}
	t.Logf("scheduler refused it: %s", reason)

	after, err := countThunderClients(ctx)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if after != before {
		t.Errorf("ThunderClients went from %d to %d; an unschedulable claim must not enrol anything", before, after)
	}
}

// thunderClientForClaim reports whether a per-claim record exists for a claim.
func thunderClientForClaim(ctx context.Context, claimName string) (bool, error) {
	list, err := dynClient.Resource(thunderClientGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list ThunderClients: %w", err)
	}
	for _, item := range list.Items {
		spec, ok := item.Object["spec"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := spec["claimName"].(string); name == claimName {
			return true, nil
		}
	}
	return false, nil
}

// uniqueName keeps parallel runs and reruns from colliding.
func uniqueName(prefix string) string {
	return fmt.Sprintf("e2e-%s-%d", prefix, time.Now().UnixNano()%1e9)
}
