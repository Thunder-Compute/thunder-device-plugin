# Thunder Device Plugin

Attach [Thunder Compute](https://www.thundercompute.com) GPUs to Kubernetes pods
and KubeVirt VMs through [Dynamic Resource
Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/).
GPUs are pooled per zone, not pinned to a node.

```yaml
resources:
  limits:
    thundercompute.com/gpu-a6000: 2   # served from the zone pool
```

```bash
make install      # needs THUNDER_API_TOKEN
make test-local   # full test on a throwaway kind cluster, no GPU needed
```

## Contents

- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Install](#install)
- [Requesting GPUs](#requesting-gpus)
  - [Extended resources](#extended-resources)
  - [ResourceClaims](#resourceclaims)
  - [KubeVirt VMs](#kubevirt-vms)
- [GPU types](#gpu-types)
- [Oversubscription](#oversubscription)
- [Node setup](#node-setup)
- [Feature gates](#feature-gates)
- [Configuration](#configuration)
- [Development](#development)
- [Troubleshooting](#troubleshooting)
- [Releases](#releases)
- [Repository layout](#repository-layout)

## How it works

Two components:

| | What it does |
| --- | --- |
| **operator** (Deployment) | Reads Thunder inventory. Publishes one `ResourceSlice` device per GPU, one pool per zone + GPU type, and one `DeviceClass` per GPU type. |
| **daemon** (DaemonSet) | Enrolls the node with Thunder, serves the DRA kubelet plugin, mints a client token per claim, writes CDI specs and VM guest artifacts, and stages the Thunder client into each container. |

```text
Thunder inventory → ResourceSlice + DeviceClass → scheduler allocates
                                                → kubelet prepare
                                                → ThunderClient + CDI / guest artifacts
                                                → CDI hook stages the client
                                                  into the container
```

Pods need no Thunder client in their image. A CDI hook installs `libthunder.so`
and its config into each container as it is created, so a stock `ubuntu` image
works unmodified — no shell, no `curl` and no root required of the workload.
The library is downloaded once per node from `thunder.artifactBaseURL`, verified
against the digest its installer pins, and cached by digest.

Everything lives under one domain:

| Concept | Name |
| --- | --- |
| DRA driver | `thundercompute.com` |
| Extended resource | `thundercompute.com/gpu-<type>` |
| Per-type `DeviceClass` | `thunder-gpu-<type>` (generated) |
| Device attributes | `thundercompute.com/gpu_type`, `thundercompute.com/zone` |
| Device names | `<gpu-type>-<n>`, one per GPU |
| Oversubscription target | `thundercompute.com/oversubscription` (slice label) |
| CDI device | `thundercompute.com/gpu=claim-<uid>` |
| Per-claim resource | `clients.thundercompute.com` |

## Requirements

| | |
| --- | --- |
| Kubernetes 1.34+ | Default config uses only GA DRA APIs — no feature gates |
| Kubernetes 1.36+ | Needed only for `resources.limits` requests ([why](#feature-gates)) |
| NVIDIA driver 610+ | On GPU-serving nodes only |
| Thunder API token | Permissions for zones, servers, clients, enrollment tokens |
| KubeVirt + CDI | Only for VM workloads |

Consuming nodes need no local GPU. Serving nodes do.

## Install

```bash
kubectl create namespace thunder-system
kubectl -n thunder-system create secret generic thunder-api \
  --from-literal=THUNDER_API_TOKEN='<token>'

helm install thunder-device-plugin \
  oci://ghcr.io/thunder-compute/charts/thunder-device-plugin \
  --namespace thunder-system --version <version>
```

Releases are listed under
[Releases](https://github.com/Thunder-Compute/thunder-device-plugin/releases).
To install from a clone instead, point Helm at
`charts/thunder-device-plugin` and apply
`charts/thunder-device-plugin/crds/` first.

The chart never takes the API token as a value, so it never reaches the Helm
release history. Create the Secret first; point at a different name with
`--set thunder.secretName=<name>`.

Verify:

```bash
helm test thunder-device-plugin -n thunder-system
kubectl get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver \
  -o custom-columns=CLASS:.metadata.name,RESOURCE:.spec.extendedResourceName
```

```text
CLASS               RESOURCE
thunder-gpu-a6000   thundercompute.com/gpu-a6000
thunder-gpu-h100    thundercompute.com/gpu-h100
```

One class per GPU model — see [GPU types](#gpu-types).

Helm never upgrades `crds/`, so re-apply them on every chart upgrade:

```bash
helm show crds oci://ghcr.io/thunder-compute/charts/thunder-device-plugin \
  --version <version> | kubectl apply -f -
```

## Requesting GPUs

| | Syntax | GPU type from | Requires |
| --- | --- | --- | --- |
| [Extended resource](#extended-resources) | `resources.limits` | the resource name | 1.36+ |
| [`ResourceClaim`](#resourceclaims) | `count:` | the `DeviceClass` name | 1.34+ |

Both draw from the same zone pool.

### Extended resources

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: thunder-simple
spec:
  containers:
    - name: tester
      image: ubuntu:24.04
      command: ["bash", "-lc", "nvidia-smi && sleep 3600"]
      resources:
        limits:
          thundercompute.com/gpu-a6000: 2
```

The scheduler generates the `ResourceClaim` for you. Nothing appears in
`kubectl describe node` — the kubelet does not advertise these, the scheduler
resolves them. That is what lets them be pooled across a zone.

### ResourceClaims

Pods use `ResourceClaimTemplate`, so Kubernetes creates and deletes one claim per
pod:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: thunder-pod-gpu
spec:
  spec:
    devices:
      requests:
        - name: gpu
          exactly:
            deviceClassName: thunder-gpu-a6000   # the class pins the model
            allocationMode: ExactCount
            count: 2                             # one device per GPU
---
apiVersion: v1
kind: Pod
metadata:
  name: thunder-pod-gpu
spec:
  restartPolicy: Never
  resourceClaims:
    - name: gpu
      resourceClaimTemplateName: thunder-pod-gpu
  containers:
    - name: tester
      image: ubuntu:24.04
      command: ["bash", "-lc", "nvidia-smi && sleep 3600"]
      resources:
        claims:
          - name: gpu
            request: gpu
```

The generated `DeviceClass` supplies the installation default from
`hostArtifacts.defaultProfile`. A workload can override it per claim with an
opaque DRA configuration:

```yaml
    devices:
      requests:
        - name: gpu
          exactly:
            deviceClassName: thunder-gpu-a6000
            allocationMode: ExactCount
            count: 1
      config:
        - requests: [gpu]
          opaque:
            driver: thundercompute.com
            parameters:
              apiVersion: thundercompute.com/v1alpha1
              kind: ThunderDeviceConfig
              hostArtifacts:
                profile: full
```

Profiles are `none` (no host GPU artifacts), `driver` (`nvidia-smi`,
`libcuda.so.1`, and `libnvidia-ml.so.1`), and `full` (the driver artifacts plus
the configured toolkit directory). Host paths are controlled only by the
cluster administrator. The Thunder client staging hook is present for all three
profiles. A selected profile whose artifacts are missing fails claim preparation
instead of starting an incomplete container.

Use `driver` when the image already contains its toolkit, `full` when it needs
the host toolkit, and `none` only when the image supplies all required GPU
artifacts. Because one claim produces one CDI device, all requests in a
multi-request claim must resolve to the same profile.

Or use the test chart:

```bash
helm install thunder-pod charts/tests/pod --set gpu.type=A6000 --set gpu.count=2
```

### KubeVirt VMs

A VM declares a `ResourceClaim` with a name you choose. When the VM starts, the
daemon writes a Secret named after that claim:

```text
<claim-name>-thunder-setup   # enrollment-token, install-thunder-client.sh
```

The VM mounts it over virtiofs and cloud-init runs the script, which installs
the Thunder client into the guest. The GPU is reached over the network from
there, so the VM declares no GPU device.

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: thunder-vm-claim
spec:
  devices:
    requests:
      - name: gpu
        exactly:
          deviceClassName: thunder-gpu-a6000
          allocationMode: ExactCount
          count: 1
---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: thunder-vm
spec:
  running: true
  template:
    spec:
      resourceClaims:
        - name: gpu
          resourceClaimName: thunder-vm-claim
      domain:
        resources:
          requests:
            memory: 1Gi
        devices:
          filesystems:
            - name: thunder-setup
              virtiofs: {}
          disks:
            - name: cloudinitdisk
              disk:
                bus: virtio
      volumes:
        - name: thunder-setup
          secret:
            secretName: thunder-vm-claim-thunder-setup
        - name: cloudinitdisk
          cloudInitNoCloud:
            userData: |
              #cloud-config
              mounts:
                - [ thunder-setup, /mnt/thunder-setup, virtiofs ]
              runcmd:
                - [ bash, /mnt/thunder-setup/install-thunder-client.sh ]
```

Full VM with root disk and networking:

```bash
helm install thunder-vm charts/tests/vm --set gpu.type=A6000 --set gpu.count=1
```

## GPU types

Each GPU model gets its own `DeviceClass` and extended resource, generated from
Thunder inventory. **Kubernetes labels never carry the GPU model** — it is
detected on the host and reported by Thunder:

```text
1. thunderd detects the GPUs on the host        4x A6000
2. Thunder API inventory reports them           {gpuType: "A6000", gpuCount: 4}
3. operator groups inventory by zone + model    local-zone / a6000
4. operator creates the class and resource      thunder-gpu-a6000
                                                thundercompute.com/gpu-a6000
```

The daemon does step 1 indirectly: it runs the Thunder installer on the host,
and `thunderd` inspects the hardware and registers it. The daemon's own
`nvidia-smi` checks only verify that a GPU and a recent enough driver exist — it
never reads the model, and neither does the operator.

So a model no zone has served before becomes requestable within one reconcile
(default `60s`) of the node being **enrolled with Thunder**. Adding a Kubernetes
node is not the trigger. Retiring the last GPU of a model removes its class and
pool the same way.

```bash
kubectl get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver --watch
```

**Every request names a model**, because a Thunder client is enrolled with one
GPU model:

```yaml
resources:
  limits:
    thundercompute.com/gpu-a6000: 2
```

Notes: it polls rather than watches, so new hardware appears within
`operator.reconcileInterval`. A model that leaves inventory entirely has its class
removed — running claims are unaffected, new pods pend. Requesting a model you do
not own yet leaves the pod `Pending` rather than failing.

## Oversubscription

How many GPUs a zone publishes is **capacity policy from the Thunder API**, not a
chart setting. The operator reads each zone's oversubscription target per GPU
model and publishes that many devices:

```text
4 physical A6000s x 1.5 target  ->  6 published GPUs
```

Change the target in Thunder and the pool resizes on the next reconcile — no
redeploy. Targets round down, so a target is never exceeded, and a missing or
malformed target falls back to `1` rather than emptying a zone. If the API call
fails the operator logs a warning and assumes `1` rather than failing inventory.

A claim always gets whole GPUs; sharing is expressed by how many the zone
publishes.

```bash
kubectl get resourceslices -l app.kubernetes.io/name=thunder-dra-driver \
  -o custom-columns=POOL:.spec.pool.name,TARGET:'.metadata.labels.thundercompute\.com/oversubscription'
```

## Node setup

Only GPU-serving nodes need labels, and only these two:

```bash
kubectl label node <node> thundercompute.com/node=true
kubectl label node <node> topology.kubernetes.io/zone=<zone>
```

Nodes that only *consume* GPUs need nothing. The GPU model is detected on the
host, not read from a label — see [GPU types](#gpu-types) — and the daemon
verifies the GPU and driver version itself at startup, failing with a clear
error if either is missing.

**Advertised IP** — the address Thunder clients use to reach the node. It
**defaults to the node's own IP**, so most clusters configure nothing. Resolution
order:

1. node label `thundercompute.com/advertised-ip`
2. `status.addresses`: `InternalIP`, then `ExternalIP`

Override per node only when clients reach it on a different address, e.g. behind
NAT:

```bash
kubectl label node <node> thundercompute.com/advertised-ip=<reachable-ip>
```

## Feature gates

The DRA APIs are GA in 1.34 and everything the driver publishes uses only those —
**a stock 1.34+ cluster needs no gates.** One optional capability does:

| Capability | Gate | Needed for |
| --- | --- | --- |
| Extended resources | `DRAExtendedResource` | `resources.limits` requests |

It is **beta and on by default from 1.36**, so this only matters on 1.34–1.35,
where it is alpha. Without it, use `ResourceClaim`s — they need no gate. EKS, GKE
and AKS on 1.36+ work with no control plane configuration.

<details>
<summary>Enabling it on 1.34–1.35</summary>

Set on all four components — API server, scheduler, controller-manager, kubelet.
Missing the API server one is the failure worth knowing: objects are accepted but
the field is silently dropped.

**RKE2** (`/etc/rancher/rke2/config.yaml`) and **K3s** (`/etc/rancher/k3s/config.yaml`):

```yaml
kube-apiserver-arg:
  - "feature-gates=DRAExtendedResource=true"
kube-scheduler-arg:
  - "feature-gates=DRAExtendedResource=true"
kube-controller-manager-arg:
  - "feature-gates=DRAExtendedResource=true"
kubelet-arg:
  - "feature-gates=DRAExtendedResource=true"
```

Agents need only the `kubelet-arg` block. Restart `rke2-server`/`rke2-agent` or
`k3s`/`k3s-agent`.

**kubeadm** — add `--feature-gates=...` to the three static pod manifests in
`/etc/kubernetes/manifests/`, and to `/var/lib/kubelet/config.yaml` on every node:

```yaml
featureGates:
  DRAExtendedResource: true
```

If a component already has `--feature-gates`, extend that list — a second flag is
ignored.

**kind**:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  DRAExtendedResource: true
nodes:
  - role: control-plane
```

</details>

Check a cluster with `make preflight`, or by hand — the output must still contain
`extendedResourceName`:

```bash
kubectl apply --dry-run=server -o yaml -f - <<'EOF'
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: thunder-extended-resource-check
spec:
  extendedResourceName: thundercompute.com/gpu
  selectors:
    - cel:
        expression: device.driver == "thundercompute.com"
EOF
```

## Configuration

Full reference:
[`charts/thunder-device-plugin/README.md`](charts/thunder-device-plugin/README.md).
Values are validated against
[`values.schema.json`](charts/thunder-device-plugin/values.schema.json), so a
misspelled `--set` key fails the install.

| Value | Default | Purpose |
| --- | --- | --- |
| `thunder.secretName` | `thunder-api` | Existing Secret holding `THUNDER_API_TOKEN` |
| `thunder.apiURL` | `https://registry.thundercompute.com` | Thunder API endpoint |
| `thunder.artifactBaseURL` | `https://get.thundercompute.com` | Where the Thunder client library is downloaded from |
| `thunder.telemetryURL` | `https://telemetry.thundercompute.com:2096` | Telemetry collector written into each container's client config |
| `operator.extendedResourcePrefix` | `thundercompute.com/gpu-` | Prefix for per-model extended resources; `""` for clusters below 1.36 |
| `operator.reconcileInterval` | `60s` | How quickly inventory and targets are picked up |
| `operator.orphanGracePeriod` | `5m` | How long a client resource may outlive its claim before being revoked |
| `hostArtifacts.defaultProfile` | `driver` | Default host GPU artifact profile: `none`, `driver`, or `full` |
| `hostArtifacts.toolkitPath` | `/usr/local/cuda` | Read-only host toolkit directory used by the `full` profile |
| `nvidia.minDriverVersion` | `610` | Daemon refuses older drivers |
| `kubelet.pluginDirRoot` | `/var/lib/kubelet/plugins` | Change on distributions that move the kubelet root |

## Development

```bash
make help          # all targets
make check         # lint + unit tests + offline chart verification
make test-local    # integration test on a throwaway kind cluster
make image         # multi-stage builds from source
```

| Target | What it does |
| --- | --- |
| `make lint` / `test` / `cover` | `gofmt` + `go vet`, unit tests, coverage |
| `make build` | Both binaries into `bin/`, version linked in |
| `make image` / `push` / `image-buildx` | Container images |
| `make helm-lint` / `helm-schema` / `helm-package` | Chart checks and packaging |
| `make verify` / `test-local` / `preflight` | See [`hack/`](hack/) |
| `make install` / `uninstall` / `status` / `logs` | Operate a deployment |
| `make purge` | Remove the release *and* everything the driver wrote at runtime |

`make uninstall` and `make purge` name the cluster and wait for you to type its
context back. Both refuse outright while any `clients.thundercompute.com` still
exists, because removing the driver then would strand Thunder enrollments with
nothing to revoke them. Override with `FORCE=1`, skip the prompt with `YES=1`.

`make test-local` needs `docker` and [kind](https://kind.sigs.k8s.io) — no
existing cluster, Thunder account, API token or GPU. It runs the real operator
against a stub Thunder API and drives real claims through scheduling. It cannot
cover the daemon: preparing a claim needs a real GPU and a Thunder enrollment.
Details in [`hack/README.md`](hack/README.md).

## Troubleshooting

| Symptom | Cause | Check |
| --- | --- | --- |
| Claims stay `Pending` | No slice for that GPU type | `kubectl get resourceslices -l app.kubernetes.io/name=thunder-dra-driver` |
| `thundercompute.com/gpu-x` unknown | No node of that model is enrolled | `kubectl get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver` |
| Extended resource ignored | `DRAExtendedResource` off (1.34–1.35) | `make preflight` |
| Pod stuck `ContainerCreating` | Daemon failing to prepare | `kubectl -n thunder-system logs -l app.kubernetes.io/component=daemon` |
| Unknown resource name | Model not enrolled, or name misspelled | `kubectl get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver` |
| Pool bigger than the GPU count | Zone oversubscription target above 1 | See [Oversubscription](#oversubscription) |
| Daemon will not start | No zone label, or no node IP | `kubectl get node <node> -o wide` |
| VM boots without a GPU | Guest artifacts not mounted | `kubectl get cm,secret \| grep <claim-name>-thunder` |
| Client resource stuck `Terminating` | Its `ResourceClaim` still exists; the finalizer holds it until the enrollment is revoked | `kubectl get clients.thundercompute.com -A -o wide` |
| No slices at all | Operator cannot reach Thunder | `kubectl -n thunder-system logs -l app.kubernetes.io/component=operator` |

Daemon pods republish thunderd's own journal from the node, one line at a time
under a `thunderd:` prefix, so what thunderd did is visible next to what the
daemon decided about it:

```bash
kubectl -n thunder-system logs -l app.kubernetes.io/component=daemon | grep '^thunderd:'
```

Set `THUNDERD_LOG_UNIT=off` on the daemon container to leave those logs on the
node, or to another unit name if thunderd runs as one.

## Releases

CI publishes and signs images for changes merged into `next`, then opens a
reviewable PR that records those source-commit tags in `values.yaml`. The
release workflow verifies those exact image references and packages the chart
without rebuilding or retagging images.

Contributors: see
[CONTRIBUTING.md](CONTRIBUTING.md) and [docs/RELEASING.md](docs/RELEASING.md).

## Repository layout

| Path | Contents |
| --- | --- |
| [`cmd/`](cmd) | Daemon and operator entrypoints |
| [`internal/daemon`](internal/daemon) | Node enrollment, DRA kubelet plugin, CDI and guest artifacts |
| [`internal/operator`](internal/operator) | `ResourceSlice` and `DeviceClass` reconciliation |
| [`internal/version`](internal/version) | Build version and Thunder API user agent |
| [`charts/thunder-device-plugin`](charts/thunder-device-plugin) | Main chart |
| [`charts/tests`](charts/tests) | Pod and VM test charts |
| [`containers/`](containers) | Multi-stage Dockerfiles |
| [`hack/`](hack) | Verification, release and test scripts |
| [`test/e2e`](test/e2e) | End-to-end tests against a live cluster |
| [`Makefile`](Makefile) | Everything above |

The Thunder API client is the external
[`github.com/Thunder-Compute/thunder-sdk`](https://github.com/Thunder-Compute/thunder-sdk)
module.
