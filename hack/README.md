# hack

Development and test scripts. Every script is self-documenting — run it with
`--help`. All of them are wired into the [Makefile](../Makefile).

| Script | Needs | What it does |
| --- | --- | --- |
| [`verify.sh`](verify.sh) | nothing | Builds, vets and tests the Go code, renders every chart, and asserts the manifests carry the expected resource identity and packaging. |
| [`test-local.sh`](test-local.sh) | docker, kind | Creates a throwaway local cluster and tests the operator and the whole scheduling path against it. |
| [`preflight.sh`](preflight.sh) | a cluster | **Not a test.** A read-only diagnostic you run against a cluster you are about to deploy to. |

Shared helpers live in [`lib/common.sh`](lib/common.sh) (logging, assertions,
retries) and [`lib/thunder.sh`](lib/thunder.sh) (resource names and lookups).
[`thunder-registry-stub.go`](thunder-registry-stub.go) is a stand-in for the
Thunder Registry API used by the local test. It runs outside the cluster on
purpose: it is scaffolding, not a component, so keeping it out keeps the
deployed manifests honest. It records state-changing requests instead of
applying them, and serves them back on `/debug/writes`.

Its inventory is generated at run time into the test's temp directory
(`inventory.json`); there is no checked-in fixture. `--keep` prints the path.

## Offline checks

```bash
make verify
```

Needs `go` and `helm`, and passes with no cluster reachable. This is the check
to run in CI and before pushing.

## Local integration test

```bash
make test-local
```

Requires `docker`, `go`, `jq`, `helm` and
[kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation).
It needs **no existing cluster, no Thunder account, no API token and no GPU**,
and it deploys nothing to any real infrastructure.

What it does:

1. Creates a kind cluster (`kindest/node:v1.36.1` by default) and deletes it
   again at the end.
2. Confirms the API server preserves the extended resource mapping.
3. Installs the chart, which exercises the CRDs, RBAC and the values schema
   against a real API server.
4. Builds and starts the registry stub, serving synthetic inventory: initially
   4x A6000 in one zone at a `1x` oversubscription target. It re-reads its
   inventory file per request, so the test can enroll hardware and change
   capacity policy mid-run, and it announces its own port rather than the
   script guessing a free one.
5. Builds both container images, loads them into the cluster, and installs the
   chart. The operator then runs **in cluster, from the chart** — its image,
   env, ServiceAccount and RBAC — so a missing permission fails here instead of
   being masked. It also runs the chart's own `helm test`. Then asserts
   the published `ResourceSlice`: driver, one device per GPU, device naming,
   zone attribute and shard count.
6. Asserts the operator made **no writes** to Thunder. Creating zones and
   enrolling hardware is the node daemon's job; the operator only reads.
7. Runs the chart's own `helm test` in cluster.
8. Installs the pod test chart, and asserts the scheduler allocates two
   distinct GPU devices to the claim from the right pool.
9. Asserts the operator generated a `DeviceClass` per GPU type, each pinning its
   model with a CEL selector and exposing its own extended resource.
10. Adds a second GPU model (H100) to inventory and asserts the operator creates
   its class, extended resource and pool on its own, and that a pod can request
   the new resource immediately.
11. Asserts a `resources.limits: thundercompute.com/gpu-a6000: 2` pod is served
   from the A6000 zone pool, gets two GPUs of that model only, and that the
   resource is scheduler-resolved rather than advertised in node `allocatable`.
12. Asserts a typed request larger than that model's supply stays pending
    instead of borrowing the other model.
13. Asserts a request for more GPUs than the zone has never allocates.
14. Retires the H100s and asserts the class and pool are pruned, leaving the
    other model untouched.
15. Raises the zone's oversubscription target in the stub API to `1.5` and
    asserts the pool grows from 4 to 6 GPUs, records the target on the slice,
    keeps the devices exclusive, and shrinks again when the target returns to 1.

### What it cannot cover

The node daemon. Preparing a claim needs a real GPU, a Thunder enrollment and a
registered kubelet plugin, so on kind a pod stops at `ContainerCreating` once
its claim is allocated. Everything downstream of allocation — CDI specs,
`ThunderClient` resources, VM guest artifacts — is only exercised on a real
Thunder GPU node.

The daemon's calls to Thunder are covered by Go tests instead, in
`internal/daemon/enrollment_test.go`: zone creation, server enrollment (and the
installer command it produces), client enrollment, and revocation, each against
an HTTP server that records what was sent.

### Options

```bash
hack/test-local.sh --keep                     # leave the cluster up to inspect
hack/test-local.sh --reuse                    # reuse an existing cluster, faster
hack/test-local.sh --node-image kindest/node:v1.34.0
hack/test-local.sh --cluster my-cluster
```

The kind config sets `DRAExtendedResource` explicitly. It is beta and on by
default from Kubernetes 1.36, but setting it keeps older node images working.

On failure the script prints the operator log, slices, claims and pods before
cleaning up.

## Preflight

```bash
make preflight
```

Checks a cluster you already have: DRA APIs served, extended resource mapping
preserved by the API server, Thunder node labels, and — with `--with-vm` —
KubeVirt and CDI. It creates nothing and is safe to run
against production.

## Environment

| Variable | Purpose |
| --- | --- |
| `KUBECONFIG`, `KUBE_CONTEXT` | Select the target cluster (`preflight.sh` only; the local test makes its own) |
| `KUBECTL`, `HELM`, `KIND` | Override the binaries used |
| `NODE_IMAGE`, `CLUSTER` | Local test cluster image and name |
| `NO_COLOR` | Disable coloured output |
