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
- [Node setup](#node-setup)
- [Feature gates](#feature-gates)
- [Configuration](#configuration)
- [Development](#development)
- [Troubleshooting](#troubleshooting)
- [Repository layout](#repository-layout)

## How it works

Two components:

| | What it does |
| --- | --- |
| **operator** (Deployment) | Reads Thunder inventory. Publishes one `ResourceSlice` device per GPU, one pool per zone + GPU type, and one `DeviceClass` per GPU type. |
| **daemon** (DaemonSet) | Enrolls the node with Thunder, serves the DRA kubelet plugin, mints a client token per claim, writes CDI specs and VM guest artifacts. |

```text
Thunder inventory → ResourceSlice + DeviceClass → scheduler allocates
                                                → kubelet prepare
                                                → ThunderClient + CDI / guest artifacts
```

Everything lives under one domain:

| Concept | Name |
| --- | --- |
| DRA driver | `thundercompute.com` |
| Extended resource | `thundercompute.com/gpu-<type>` |
| Per-type `DeviceClass` | `thunder-gpu-<type>` (generated) |
| Catch-all `DeviceClass` | `thunder-gpu` (for claims with your own selectors) |
| Device attributes | `thundercompute.com/gpu_type`, `thundercompute.com/zone` |
| Device names | `<gpu-type>-<n>`, one per GPU |
| Oversubscription capacity | `thundercompute.com/shares` (only when `sharesPerGPU > 1`) |
| CDI device | `thundercompute.com/gpu=claim-<uid>` |
| Per-claim resource | `clients.thundercompute.com` |

## Requirements

| | |
| --- | --- |
| Kubernetes 1.34+ | Default config uses only GA DRA APIs — no feature gates |
| Kubernetes 1.36+ | Needed for `resources.limits` requests and GPU oversubscription ([why](#feature-gates)) |
| NVIDIA driver 610+ | On GPU-serving nodes only |
| Thunder API token | Permissions for zones, servers, clients, enrollment tokens |
| KubeVirt + CDI | Only for VM workloads |

Consuming nodes need no local GPU. Serving nodes do.

## Install

```bash
kubectl create namespace thunder-system
kubectl -n thunder-system create secret generic thunder-api \
  --from-literal=THUNDER_API_TOKEN='<token>'

kubectl apply -f charts/thunder-device-plugin/crds/

helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system \
  --set namespace.create=false \
  --set thunder.existingSecret=thunder-api
```

Verify:

```bash
helm test thunder-device-plugin -n thunder-system
kubectl get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver \
  -o custom-columns=CLASS:.metadata.name,RESOURCE:.spec.extendedResourceName
```

```text
CLASS               RESOURCE
thunder-gpu         <none>
thunder-gpu-a6000   thundercompute.com/gpu-a6000
thunder-gpu-h100    thundercompute.com/gpu-h100
```

Helm never upgrades `crds/`, so re-apply it on every chart upgrade.

## Requesting GPUs

| | Syntax | GPU type from | Requires |
| --- | --- | --- | --- |
| [Extended resource](#extended-resources) | `resources.limits` | the resource name | 1.36+ |
| [`ResourceClaim`](#resourceclaims) | `count:` + selector | a CEL selector | 1.34+ |

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
            deviceClassName: thunder-gpu
            allocationMode: ExactCount
            count: 2                    # one device per GPU
            selectors:
              - cel:
                  expression: device.attributes["thundercompute.com"]["gpu_type"] == "A6000"
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

Or use the test chart:

```bash
helm install thunder-pod charts/tests/pod --set gpu.type=A6000 --set gpu.count=2
```

### KubeVirt VMs

VMs need a **stable** `ResourceClaim`, not a template, because the claim name
determines the guest artifacts the daemon creates:

```text
<claim-name>-thunder-configmap   # install-thunder-client.sh
<claim-name>-thunder-secret      # enrollment-token
```

Enable the KubeVirt gate once:

```bash
kubectl -n kubevirt patch kubevirt kubevirt --type merge \
  -p '{"spec":{"configuration":{"developerConfiguration":{"featureGates":["GPUsWithDRA"]}}}}'
```

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
          deviceClassName: thunder-gpu
          allocationMode: ExactCount
          count: 1
          selectors:
            - cel:
                expression: device.attributes["thundercompute.com"]["gpu_type"] == "A6000"
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
            - name: thunder-config
              virtiofs: {}
            - name: thunder-secret
              virtiofs: {}
          disks:
            - name: cloudinitdisk
              disk:
                bus: virtio
      volumes:
        - name: thunder-config
          configMap:
            name: thunder-vm-claim-thunder-configmap
            optional: true
        - name: thunder-secret
          secret:
            secretName: thunder-vm-claim-thunder-secret
            optional: true
        - name: cloudinitdisk
          cloudInitNoCloud:
            userData: |
              #cloud-config
              mounts:
                - [ thunder-config, /mnt/thunder-config, virtiofs ]
                - [ thunder-secret, /mnt/thunder-secret, virtiofs ]
              runcmd:
                - [ bash, /mnt/thunder-config/install-thunder-client.sh ]
```

Full VM with root disk and networking:

```bash
helm install thunder-vm charts/tests/vm --set gpu.type=A6000 --set gpu.count=1
```

## GPU types

**Resource types are generated from Thunder inventory.** Enroll a node with a GPU
model no zone has served before, and its class, pool and extended resource appear
within one reconcile (default `60s`). Retire the last one and they are removed.

```text
label node → daemon enrolls it → Thunder reports the host → operator reconciles
                                                          → thunder-gpu-h100
                                                          → thundercompute.com/gpu-h100
```

Adding a Kubernetes node is not the trigger; enrolling it with Thunder is.

```bash
kubectl get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver --watch
```

**Every request must pin a GPU type.** A zone can serve several models, and one
Thunder client is enrolled with exactly one model, so a mixed claim is unservable.

- **Extended resources** pin it by name: `thundercompute.com/gpu-a6000`.
  A `DeviceClass` has only per-device selectors and no `matchAttribute`
  constraint, so a *catch-all* `thundercompute.com/gpu` could return a mix of
  models. That is why the chart ships `thunder-gpu` with no extended resource
  name. Turn it on only if every zone serves one model:
  `--set deviceClass.extendedResourceName=thundercompute.com/gpu`.
- **Claims** pin it with a CEL selector, which is evaluated per device. Writing a
  multi-GPU claim with no type selector? Add a constraint:

  ```yaml
  spec:
    devices:
      constraints:
        - matchAttribute: thundercompute.com/gpu_type
  ```

Notes: it polls rather than watches, so new hardware appears within
`operator.reconcileInterval`. A model that leaves inventory entirely has its class
removed — running claims are unaffected, new pods pend. Requesting a model you do
not own yet leaves the pod `Pending` rather than failing.

## Node setup

Only GPU-serving nodes need labels:

```bash
kubectl label node <node> thundercompute.com/node=true
kubectl label node <node> topology.kubernetes.io/zone=<zone>
kubectl label node <node> nvidia.com/gpu.present=true
```

**Advertised IP** — the address Thunder clients use to reach the node. It
**defaults to the node's own IP**, so most clusters configure nothing. Resolution
order:

1. `--set node.advertisedIP=<ip>` (applies to every node in the release)
2. node label `thundercompute.com/advertised-ip`
3. `status.addresses`: `InternalIP`, then `ExternalIP`

Override per node only when clients reach it on a different address, e.g. behind
NAT:

```bash
kubectl label node <node> thundercompute.com/advertised-ip=<reachable-ip>
```

## Feature gates

The DRA APIs are GA in 1.34 and the default configuration uses nothing else — **a
stock 1.34+ cluster needs no gates.** Two optional capabilities do:

| Capability | Gate | Needed for |
| --- | --- | --- |
| Extended resources | `DRAExtendedResource` | `resources.limits` requests |
| GPU oversubscription | `DRAConsumableCapacity` | `operator.sharesPerGPU > 1` |

Both are **beta and on by default from 1.36**, so this only matters on 1.34–1.35,
where they are alpha. EKS, GKE and AKS on 1.36+ work with no control plane
configuration.

<details>
<summary>Enabling them on 1.34–1.35</summary>

Set on all four components — API server, scheduler, controller-manager, kubelet.
Missing the API server one is the failure worth knowing: objects are accepted but
the field is silently dropped.

**RKE2** (`/etc/rancher/rke2/config.yaml`) and **K3s** (`/etc/rancher/k3s/config.yaml`):

```yaml
kube-apiserver-arg:
  - "feature-gates=DRAExtendedResource=true,DRAConsumableCapacity=true"
kube-scheduler-arg:
  - "feature-gates=DRAExtendedResource=true,DRAConsumableCapacity=true"
kube-controller-manager-arg:
  - "feature-gates=DRAExtendedResource=true,DRAConsumableCapacity=true"
kubelet-arg:
  - "feature-gates=DRAExtendedResource=true,DRAConsumableCapacity=true"
```

Agents need only the `kubelet-arg` block. Restart `rke2-server`/`rke2-agent` or
`k3s`/`k3s-agent`.

**kubeadm** — add `--feature-gates=...` to the three static pod manifests in
`/etc/kubernetes/manifests/`, and to `/var/lib/kubelet/config.yaml` on every node:

```yaml
featureGates:
  DRAExtendedResource: true
  DRAConsumableCapacity: true
```

If a component already has `--feature-gates`, extend that list — a second flag is
ignored.

**kind**:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  DRAExtendedResource: true
  DRAConsumableCapacity: true
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
| `operator.sharesPerGPU` | `1` | Clients sharing one GPU; `>1` needs 1.36+ |
| `operator.extendedResourcePrefix` | `thundercompute.com/gpu-` | Prefix for generated per-type resources; `""` disables |
| `operator.reconcileInterval` | `60s` | How quickly new hardware appears |
| `deviceClass.extendedResourceName` | `""` | Catch-all resource; unsafe with multiple GPU types |
| `node.advertisedIP` | `""` | Pins the advertised IP for every node |
| `nvidia.minDriverVersion` | `610` | Daemon refuses older drivers |
| `thunder.existingSecret` | `""` | Reuse a pre-created token Secret |

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
| `claim mixes Thunder pools` | Claim spans GPU models | Pin a type — see [GPU types](#gpu-types) |
| Daemon will not start | No zone label, or no node IP | `kubectl get node <node> -o wide` |
| VM boots without a GPU | Guest artifacts not mounted | `kubectl get cm,secret \| grep <claim-name>-thunder` |
| No slices at all | Operator cannot reach Thunder | `kubectl -n thunder-system logs -l app.kubernetes.io/component=operator` |

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
| [`hack/`](hack) | Verification and test scripts |
| [`Makefile`](Makefile) | Everything above |

The Thunder API client is the external
[`github.com/Thunder-Compute/thunder-sdk`](https://github.com/Thunder-Compute/thunder-sdk)
module.
