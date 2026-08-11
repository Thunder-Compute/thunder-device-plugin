# End-to-end tests

These run against a **real cluster that already has the chart installed**. They
allocate real GPUs and enrol real Thunder clients, then release them again.

```bash
export KUBECONFIG=/path/to/cluster.config

make test-e2e          # basic + VM, a couple of minutes
make test-e2e-stress   # concurrent churn, 5m by default
make test-e2e-all      # everything
```

The `e2e` build tag keeps them out of `go test ./...`, so `make test` and
`make check` stay offline and fast.

## What each test owns

| Test | Asserts |
| --- | --- |
| `TestGPUPodIsUsable` | A stock image with no Thunder client gets a working GPU: `nvidia-smi` answers, `libthunder.so` and `config.json` were staged in, the claim is recorded, and deleting the pod releases everything. |
| `TestMultiGPUPod` | A claim for 2 GPUs produces 2 GPUs. Skipped if the cluster publishes fewer than 2. |
| `TestOverCapacityRequestIsRejected` | Asking for more than the pool holds stays `Pending` and enrols nothing, so an impossible request cannot strand capacity. |
| `TestStress` | Concurrent create/use/delete churn, then proves the cluster came back to rest. |
| `TestKubeVirtVM` | A VM's claim is prepared, its setup Secret is written with both keys, and teardown revokes the enrolment. Skipped when KubeVirt is absent. |

## GPU discovery

Nothing is hardcoded. The suite reads `DeviceClasses` and `ResourceSlices` for
the driver and counts only devices in pools whose node selector some
schedulable node satisfies — a zone pool no node can reach publishes devices
that never schedule, and counting them would make the suite request capacity
that cannot exist.

A cluster with no usable GPU **fails** rather than skipping; the GPU path is
the entire point.

## The stress test

Workers concurrently create claims of varying size, wait for them to run,
verify the GPU on a sample of rounds, hold them briefly, and delete them. One
round in ten deliberately asks for more than the pool holds.

While it runs, a watchdog asserts the driver's own pods stay ready and never
restart. When it finishes, the suite asserts that:

- every Thunder client was revoked and every claim released,
- the driver's restart count is unchanged,
- a fresh GPU pod still works.

A leak is invisible under light use and starves a zone under real load, which
is why the teardown assertions matter more than the throughput.

```bash
make test-e2e-stress E2E_STRESS_DURATION=30m E2E_STRESS_WORKERS=12
```

## Flags

Pass through `E2E_FLAGS`, or directly with `go test -tags e2e`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-e2e.namespace` | `thunder-e2e` | Namespace the tests create workloads in |
| `-e2e.driver` | `thundercompute.com` | DRA driver to exercise |
| `-e2e.image` | `ubuntu:22.04` | Workload image; deliberately ships no Thunder client |
| `-e2e.ready-timeout` | `4m` | How long a pod may take to reach `Running` |
| `-e2e.gpu-timeout` | `90s` | How long before a container's GPU must answer |
| `-e2e.stress-duration` | `5m` | How long the churn runs |
| `-e2e.stress-workers` | `6` | Concurrent workers |
| `-e2e.vm-container-disk` | cirros demo image | VM boot disk; a container disk keeps the test off CDI |
| `-e2e.keep` | `false` | Leave workloads behind on failure for debugging |

## Notes

A container can start slightly before the node's `thunderd` has picked up its
new client, so `nvidia-smi` is retried until `-e2e.gpu-timeout` rather than
being called once.

Workloads are labelled `app.kubernetes.io/managed-by=thunder-e2e`, so anything
a killed run leaves behind is easy to find:

```bash
kubectl delete pod,resourceclaimtemplate -n thunder-e2e -l app.kubernetes.io/managed-by=thunder-e2e
```
