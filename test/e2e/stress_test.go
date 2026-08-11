//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stressStats is what the run reports at the end. Counters are the only state
// shared between workers, so they are atomic rather than locked.
type stressStats struct {
	created   atomic.Int64
	ran       atomic.Int64
	gpuOK     atomic.Int64
	pending   atomic.Int64
	deleted   atomic.Int64
	failures  atomic.Int64
	firstFail atomic.Pointer[string]
}

func (s *stressStats) fail(format string, args ...any) {
	s.failures.Add(1)
	message := fmt.Sprintf(format, args...)
	s.firstFail.CompareAndSwap(nil, &message)
}

// TestStress churns claims for a while and then proves the cluster came back
// to rest.
//
// The point is not throughput. It is that after a period of concurrent create,
// use and delete — including requests that cannot be satisfied — every GPU is
// released, no client enrolment leaks, and the driver's own pods are in the
// state they started in. A leak here is invisible under light use and starves
// a zone under real load.
func TestStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped under -short")
	}

	kind := inventory.largest()
	// Leave headroom so workers contend for GPUs rather than deadlocking on a
	// pool that is already full.
	maxPerClaim := kind.Capacity / 2
	if maxPerClaim < 1 {
		maxPerClaim = 1
	}
	if maxPerClaim > 4 {
		maxPerClaim = 4
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before, err := snapshotDriver(ctx)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if before.ready != before.pods || before.pods == 0 {
		t.Fatalf("driver is not healthy before the run: %s", before)
	}
	baseline, err := countThunderClients(ctx)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if baseline != 0 {
		t.Fatalf("%d ThunderClient(s) exist before the run; start from a clean cluster", baseline)
	}

	t.Logf("stressing %s for %s with %d workers, up to %d GPUs per claim (pool publishes %d); driver: %s",
		kind.GPUType, *flagStressDuration, *flagStressWorkers, maxPerClaim, kind.Capacity, before)

	deadline := time.Now().Add(*flagStressDuration)
	stats := &stressStats{}

	// A watchdog asserts the driver stays up while the churn runs. A crash
	// halfway through is the failure this test exists to catch, and waiting
	// until the end to notice would lose the timing.
	watchdogDone := make(chan struct{})
	var watchdogFail atomic.Pointer[string]
	go func() {
		defer close(watchdogDone)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				health, err := snapshotDriver(ctx)
				if err != nil {
					continue // transient API error; the next tick retries
				}
				if health.restarts > before.restarts {
					message := fmt.Sprintf("driver pods restarted during the run: %s -> %s", before, health)
					watchdogFail.CompareAndSwap(nil, &message)
					return
				}
				if health.ready < health.pods {
					message := fmt.Sprintf("driver pods stopped being ready during the run: %s", health)
					watchdogFail.CompareAndSwap(nil, &message)
					return
				}
			}
		}
	}()

	var workers sync.WaitGroup
	for worker := 0; worker < *flagStressWorkers; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			// Each worker gets its own generator, so workers do not contend on
			// one and the mix differs between them.
			random := rand.New(rand.NewPCG(uint64(worker), uint64(worker)*7919+1))
			for round := 0; time.Now().Before(deadline); round++ {
				runStressRound(ctx, t, stats, random, kind, maxPerClaim, worker, round)
			}
		}(worker)
	}
	workers.Wait()
	cancel()
	<-watchdogDone

	t.Logf("churn finished: %d created, %d ran, %d GPUs verified, %d correctly pending, %d deleted, %d failures",
		stats.created.Load(), stats.ran.Load(), stats.gpuOK.Load(),
		stats.pending.Load(), stats.deleted.Load(), stats.failures.Load())

	if message := watchdogFail.Load(); message != nil {
		t.Errorf("%s", *message)
	}
	if failures := stats.failures.Load(); failures > 0 {
		first := "unknown"
		if message := stats.firstFail.Load(); message != nil {
			first = *message
		}
		t.Errorf("%d workload failure(s) during the run; first: %s", failures, first)
	}
	if stats.ran.Load() == 0 {
		t.Fatal("no workload ever reached Running; the run proved nothing")
	}

	// The real assertion: everything went back.
	t.Run("the cluster returns to rest", func(t *testing.T) {
		cleanupNamespace(t)
		waitQuiescent(t, *flagQuiesce)

		after, err := snapshotDriver(context.Background())
		if err != nil {
			t.Fatalf("%v", err)
		}
		if after.restarts != before.restarts {
			t.Errorf("driver restarted during the run: %s -> %s", before, after)
		}
		if after.ready != after.pods || after.pods == 0 {
			t.Errorf("driver is not healthy after the run: %s", after)
		}
		t.Logf("driver after the run: %s", after)
	})

	// A cluster that survived the churn must still be able to serve a GPU.
	t.Run("a GPU still works afterwards", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		workload, err := newGPUWorkload(ctx, uniqueName("after-stress"), kind, 1)
		if err != nil {
			t.Fatalf("create workload: %v", err)
		}
		defer cleanupWorkload(t, workload)

		if err := workload.waitRunning(ctx, *flagReadyTimeout); err != nil {
			t.Fatalf("%v", err)
		}
		if _, err := workload.waitGPUReady(ctx, *flagGPUTimeout); err != nil {
			t.Fatalf("%v", err)
		}
	})
}

// runStressRound is one create/use/delete cycle. It never fails the test
// directly: a worker records the failure and keeps going, so the run still
// reaches the teardown assertions that matter most.
func runStressRound(ctx context.Context, t *testing.T, stats *stressStats, random *rand.Rand,
	kind gpuKind, maxPerClaim, worker, round int) {

	// One round in ten asks for more than the pool holds. Those must be
	// refused without disturbing anything else.
	overCapacity := random.IntN(10) == 0
	count := 1 + random.IntN(maxPerClaim)
	if overCapacity {
		count = kind.Capacity + 1 + random.IntN(4)
	}

	name := fmt.Sprintf("e2e-stress-%d-%d", worker, round)
	workload, err := newGPUWorkload(ctx, name, kind, count)
	if err != nil {
		if ctx.Err() == nil {
			stats.fail("create %s: %v", name, err)
		}
		return
	}
	stats.created.Add(1)
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := workload.remove(removeCtx); err != nil {
			stats.fail("delete %s: %v", name, err)
			return
		}
		stats.deleted.Add(1)
	}()

	if overCapacity {
		// Long enough for the scheduler to have decided, short enough to keep
		// the churn moving.
		time.Sleep(15 * time.Second)
		pending, _, err := workload.isPending(ctx)
		if err != nil {
			if ctx.Err() == nil {
				stats.fail("read %s: %v", name, err)
			}
			return
		}
		if !pending {
			stats.fail("%s asked for %d GPUs from a pool of %d and was not refused", name, count, kind.Capacity)
			return
		}
		stats.pending.Add(1)
		return
	}

	if err := workload.waitRunning(ctx, *flagReadyTimeout); err != nil {
		// A pod that cannot fit right now is contention, not a fault: other
		// workers hold the GPUs. Only a hard failure counts.
		if ctx.Err() != nil {
			return
		}
		pending, reason, readErr := workload.isPending(context.Background())
		if readErr == nil && pending {
			t.Logf("%s waited for capacity and was cleaned up: %s", name, reason)
			return
		}
		stats.fail("%s never ran: %v", name, err)
		return
	}
	stats.ran.Add(1)

	// Verify the GPU on a sample of rounds. Every round would spend the whole
	// run waiting out the roster window instead of churning.
	if random.IntN(3) == 0 {
		if _, err := workload.waitGPUReady(ctx, *flagGPUTimeout); err != nil {
			if ctx.Err() == nil {
				stats.fail("%s: %v", name, err)
			}
			return
		}
		stats.gpuOK.Add(1)
	}

	// Hold the GPUs for a moment so claims overlap rather than running in
	// lockstep.
	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(random.IntN(8)+2) * time.Second):
	}
}

// cleanupNamespace removes everything the suite created, so the teardown
// assertions measure the driver rather than leftovers.
func cleanupNamespace(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	selector := metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=thunder-e2e"}
	background := metav1.DeletePropagationBackground
	options := metav1.DeleteOptions{PropagationPolicy: &background}

	if err := kubeClient.CoreV1().Pods(*flagNamespace).
		DeleteCollection(ctx, options, selector); err != nil {
		t.Errorf("delete stress pods: %v", err)
	}
	if err := kubeClient.ResourceV1().ResourceClaimTemplates(*flagNamespace).
		DeleteCollection(ctx, options, selector); err != nil {
		t.Errorf("delete stress claim templates: %v", err)
	}

	eventually(t, 3*time.Minute, "every stress pod to go away", func(ctx context.Context) error {
		pods, err := kubeClient.CoreV1().Pods(*flagNamespace).List(ctx, selector)
		if err != nil {
			return err
		}
		if len(pods.Items) != 0 {
			return fmt.Errorf("%d pod(s) still terminating", len(pods.Items))
		}
		return nil
	})
}
