# Thunder Device Plugin

Thunder Device Plugin exposes [Thunder Compute](https://thundercompute.com)
GPUs to Kubernetes through [Dynamic Resource
Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
(DRA). Pods and [KubeVirt](https://kubevirt.io) VMs request a
`thundercompute.com/gpu` the same way they would request any other DRA device,
and the driver attaches a remote Thunder GPU to the workload.

The Helm chart installs two components:

- a **cluster operator** that publishes GPU inventory as `ResourceSlice`s, and
- a **node daemon** that enrolls the node and serves the DRA kubelet plugin.

Both talk to Thunder through the official
[Thunder SDK](https://github.com/Thunder-Compute/thunder-sdk).

```bash
make help          # every available target
make check         # lint, unit tests, and chart verification
make install       # install the chart (needs THUNDER_API_TOKEN)
```

---

## Table of Contents

- [Overview](#overview)
  - [How it works](#how-it-works)
  - [Resource identity](#resource-identity)
- [Requirements](#requirements)
- [Cluster setup](#cluster-setup)
  - [Feature gates](#feature-gates)
  - [RKE2](#rke2)
  - [K3s](#k3s)
  - [kubeadm and other self-managed clusters](#kubeadm-and-other-self-managed-clusters)
  - [kind](#kind)
  - [Managed distributions](#managed-distributions)
  - [Verify the cluster](#verify-the-cluster)
- [KubeVirt setup](#kubevirt-setup)
- [Node setup](#node-setup)
  - [Required labels](#required-labels)
  - [Advertised IP](#advertised-ip)
  - [Optional labels](#optional-labels)
- [Installation](#installation)
  - [Thunder API token](#thunder-api-token)
  - [Install the chart](#install-the-chart)
  - [Upgrading](#upgrading)
  - [Verify the rollout](#verify-the-rollout)
  - [Configuration](#configuration)
- [Requesting GPUs](#requesting-gpus)
  - [Pods](#pods)
  - [Virtual machines](#virtual-machines)
  - [ResourceClaim vs ResourceClaimTemplate](#resourceclaim-vs-resourceclaimtemplate)
- [Development](#development)
  - [Make targets](#make-targets)
  - [Building images](#building-images)
  - [Testing](#testing)
- [Troubleshooting](#troubleshooting)
- [Repository layout](#repository-layout)

---

## Overview

### How it works

The **operator** publishes one fungible `ResourceSlice` per zone and GPU type.
The advertised capacity is the larger of healthy host GPUs and currently
committed Thunder clients, so active allocations stay represented even when
backing capacity changes.

The **daemon** runs on Thunder GPU nodes. It enrolls the node with the Thunder
API, serves the DRA kubelet plugin, mints one Thunder client enrollment token
per prepared `ResourceClaim`, writes pod CDI specs, and creates per-claim guest
artifacts for VMs.

```text
ResourceSlice          published by the operator, one per zone + GPU type
      |
      v
scheduler allocation   the scheduler picks a device and consumes capacity
      |
      v
ResourceClaim status   allocation result lands on the claim
      |
      v
kubelet prepare        the daemon mints a token and materializes the device
      |
      v
ThunderClient + CDI spec (pods) or ConfigMap + Secret (VMs)
```

### Resource identity

Everything the driver publishes lives under the `thundercompute.com` domain.

| Concept | Name |
| --- | --- |
| DRA driver | `thundercompute.com` |
| `DeviceClass` | `thunder-gpu` |
| Device attributes | `thundercompute.com/gpu_type`, `thundercompute.com/zone` |
| Consumable capacity | `thundercompute.com/gpu_count` |
| CDI device | `thundercompute.com/gpu=claim-<claim-uid>` |
| Node marker label | `thundercompute.com/node=true` |
| Advertised IP label | `thundercompute.com/advertised-ip` |
| Per-claim custom resource | `clients.thundercompute.com` |

---

## Requirements

| Requirement | Notes |
| --- | --- |
| Kubernetes 1.36+ recommended | 1.34 is the floor; 1.36 needs no feature-gate configuration at all |
| `DRAConsumableCapacity` | Beta and on by default from 1.36. On 1.34–1.35 it is alpha and must be enabled explicitly |
| NVIDIA driver on GPU nodes | Minimum version is `nvidia.minDriverVersion` (default `610`) |
| A Thunder API token | Needs permissions for zones, nodes, clients, and enrollment tokens |
| KubeVirt with `GPUsWithDRA` | Only for VM workloads |

To build from source you also need Go 1.25+, Helm 3.12+, and Docker. Run
`make help` for the full workflow.

Thunder GPUs are attached over the network, so a node does **not** need a local
GPU to consume one — but the nodes that *serve* GPUs do.

---

## Cluster setup

### Feature gates

Two things have to be available: the DRA APIs, and shared *consumable* capacity
so several claims can draw from one pool of GPUs.

| Kubernetes | `resource.k8s.io/v1` | `DRAConsumableCapacity` | What you must do |
| --- | --- | --- | --- |
| 1.36+ | GA | **Beta, on by default** | Nothing |
| 1.34–1.35 | GA | Alpha, off by default | Enable the gate on all four components |
| < 1.34 | not served | — | Upgrade |

**On 1.36 and newer there is nothing to configure.** A stock cluster already
preserves consumable capacity requests; the rest of this section only applies
to 1.34 and 1.35.

On 1.34–1.35 the gate must be set on all four components:

| Component | Why |
| --- | --- |
| kube-apiserver | Otherwise capacity requests are silently dropped from claims |
| kube-scheduler | Allocates and tracks consumed capacity |
| kube-controller-manager | Manages generated claims |
| kubelet | Passes consumed capacity through to the driver |

Missing the API server gate is the most common failure: claims are accepted but
the `capacity.requests` field disappears, and every allocation silently falls
back to the default count. [Verify the cluster](#verify-the-cluster) detects
exactly this.

### RKE2

> Only needed on Kubernetes 1.34–1.35.

Server config at `/etc/rancher/rke2/config.yaml`:

```yaml
kube-apiserver-arg:
  - "feature-gates=DRAConsumableCapacity=true"
kube-scheduler-arg:
  - "feature-gates=DRAConsumableCapacity=true"
kube-controller-manager-arg:
  - "feature-gates=DRAConsumableCapacity=true"
kubelet-arg:
  - "feature-gates=DRAConsumableCapacity=true"
```

Agent config at `/etc/rancher/rke2/config.yaml`:

```yaml
kubelet-arg:
  - "feature-gates=DRAConsumableCapacity=true"
```

Restart:

```bash
sudo systemctl restart rke2-server   # on servers
sudo systemctl restart rke2-agent    # on agents
```

### K3s

> Only needed on Kubernetes 1.34–1.35.

K3s takes the same flags. Server config at `/etc/rancher/k3s/config.yaml`:

```yaml
kube-apiserver-arg:
  - "feature-gates=DRAConsumableCapacity=true"
kube-scheduler-arg:
  - "feature-gates=DRAConsumableCapacity=true"
kube-controller-manager-arg:
  - "feature-gates=DRAConsumableCapacity=true"
kubelet-arg:
  - "feature-gates=DRAConsumableCapacity=true"
```

Agent config at `/etc/rancher/k3s/config.yaml`:

```yaml
kubelet-arg:
  - "feature-gates=DRAConsumableCapacity=true"
```

Restart:

```bash
sudo systemctl restart k3s           # on servers
sudo systemctl restart k3s-agent     # on agents
```

> K3s ships a stripped-down kubelet configuration. Confirm the CDI spec
> directory the daemon writes to (`/var/run/cdi` by default) is the one your
> container runtime reads; override it with `--set cdi.specDir=<path>` if not.

### kubeadm and other self-managed clusters

> Only needed on Kubernetes 1.34–1.35.

Add the gate to each control plane static pod manifest in
`/etc/kubernetes/manifests/` — `kube-apiserver.yaml`, `kube-scheduler.yaml`,
and `kube-controller-manager.yaml`:

```yaml
spec:
  containers:
    - command:
        - kube-apiserver
        - --feature-gates=DRAConsumableCapacity=true
```

The kubelet edits its own config at `/var/lib/kubelet/config.yaml`:

```yaml
featureGates:
  DRAConsumableCapacity: true
```

```bash
sudo systemctl restart kubelet
```

Editing a static pod manifest restarts that component automatically; the
kubelet needs the explicit restart.

### kind

A kind cluster on 1.36+ needs no configuration:

```bash
kind create cluster --image kindest/node:v1.36.1
```

On an older node image, enable the gate cluster-wide:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  DRAConsumableCapacity: true
nodes:
  - role: control-plane
```

A kind cluster can exercise the operator, the `DeviceClass`, and the whole
scheduling path, but it cannot enroll real Thunder GPU nodes. That is exactly
what [`hack/test-local.sh`](hack/test-local.sh) automates — see
[Testing](#testing).

### Managed distributions

Because `DRAConsumableCapacity` is beta and enabled by default from 1.36, EKS,
GKE and AKS clusters running 1.36 or newer support this driver **without any
control plane configuration** — managed providers ship beta gates on by
default, and none of them expose a way to turn this one off.

On a managed cluster still running 1.34 or 1.35 the gate is alpha, and managed
control planes do not expose alpha feature gates. Upgrade to 1.36+ rather than
trying to enable it.

Confirm with [Verify the cluster](#verify-the-cluster) before installing —
it is a server-side dry run and works on any cluster you can reach.

### Verify the cluster

The scripted check covers everything in this section:

```bash
make preflight
```

To check by hand, confirm the API server preserves capacity requests. This is a
server-side dry run, so nothing is created:

```bash
kubectl apply --dry-run=server -o yaml -f - <<'EOF'
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: thunder-capacity-field-check
  namespace: default
spec:
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
            capacity:
              requests:
                thundercompute.com/gpu_count: "1"
EOF
```

The output must still contain `thundercompute.com/gpu_count`. If the
`capacity` block is missing, the API server gate is not enabled.

Once the chart is installed, confirm the slices use shared consumable capacity:

```bash
kubectl get resourceslices -l app.kubernetes.io/name=thunder-dra-driver -o yaml \
  | grep -A12 allowMultipleAllocations
```

---

## KubeVirt setup

Skip this section if you only run pods.

KubeVirt needs its own `GPUsWithDRA` feature gate because VMs reference DRA
claims through the VMI spec:

```bash
kubectl -n kubevirt patch kubevirt kubevirt \
  --type merge \
  -p '{"spec":{"configuration":{"developerConfiguration":{"featureGates":["GPUsWithDRA"]}}}}'
```

Verify:

```bash
kubectl -n kubevirt get kubevirt kubevirt \
  -o jsonpath='{.spec.configuration.developerConfiguration.featureGates}{"\n"}'
```

Expected output includes `["GPUsWithDRA"]`.

The VM test chart boots from a `DataVolume`, so it also needs
[CDI](https://github.com/kubevirt/containerized-data-importer) installed.

---

## Node setup

The daemonset schedules only onto Thunder GPU nodes.

### Required labels

```bash
kubectl label node <node-name> thundercompute.com/node=true
kubectl label node <node-name> topology.kubernetes.io/zone=<zone-name>
kubectl label node <node-name> nvidia.com/gpu.present=true
```

The zone label maps the node onto a Thunder zone. The daemon creates the zone
through the Thunder API if it does not already exist.

Verify:

```bash
kubectl get nodes -l thundercompute.com/node=true --show-labels
```

### Advertised IP

The advertised IP is the address Thunder clients use to reach the node. **It
defaults to the node's own IP**, so most clusters need no configuration at all.

Resolution order, first match wins:

1. the `ADVERTISED_IP` environment variable (`--set node.advertisedIP=<ip>`)
2. the `thundercompute.com/advertised-ip` node label
3. the node's `status.addresses` — `InternalIP`, then `ExternalIP`

Override it per node only when clients reach the node on a different address
than the one Kubernetes records — for example behind NAT, or on a separate
data network:

```bash
kubectl label node <node-name> thundercompute.com/advertised-ip=<reachable-ip>
```

To pin the same address for every node in the release:

```bash
helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --set node.advertisedIP=<reachable-ip>
```

### Optional labels

These feed inventory and make debugging easier, but nothing requires them:

```bash
kubectl label node <node-name> thundercompute.com/gpu-count=<count>
kubectl label node <node-name> thundercompute.com/gpu-driver-version=<version>
kubectl label node <node-name> nvidia.com/gpu.product=<gpu-type>
```

---

## Installation

### Thunder API token

Both components authenticate to the Thunder API with the same token. It needs
permissions for zones, nodes, clients, and enrollment tokens.

Create the namespace and secret up front:

```bash
kubectl create namespace thunder-system
kubectl -n thunder-system create secret generic thunder-api \
  --from-literal=THUNDER_API_TOKEN='<token>' \
  --from-literal=THUNDER_API_URL='https://api.thundercompute.com:2096'
```

### Install the chart

The chart's own reference — every value, its default, and the security model —
is in
[`charts/thunder-device-plugin/README.md`](charts/thunder-device-plugin/README.md).
Values are validated against
[`values.schema.json`](charts/thunder-device-plugin/values.schema.json), so a
misspelled `--set` key fails the install instead of silently doing nothing.

With an existing secret (recommended):

```bash
helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system \
  --set namespace.create=false \
  --set namespace.name=thunder-system \
  --set thunder.existingSecret=thunder-api
```

Or let the chart create the secret from a token value:

```bash
helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system \
  --create-namespace \
  --set thunder.apiToken='<token>'
```

### Upgrading

Helm does not upgrade files in `crds/` during a normal chart upgrade. Apply
them explicitly when upgrading an existing release:

```bash
kubectl apply -f charts/thunder-device-plugin/crds/
```

### Verify the rollout

```bash
kubectl -n thunder-system rollout status deployment/thunder-device-plugin-operator
kubectl -n thunder-system rollout status daemonset/thunder-device-plugin
kubectl get deviceclasses thunder-gpu
kubectl get resourceslices -l app.kubernetes.io/name=thunder-dra-driver
```

A slice per zone and GPU type means the operator reached the Thunder API and
published inventory. The chart also ships a smoke test that waits for exactly
that:

```bash
helm test thunder-device-plugin --namespace thunder-system
```

### Configuration

The full set of values lives in
[`charts/thunder-device-plugin/values.yaml`](charts/thunder-device-plugin/values.yaml).
The ones worth knowing:

| Value | Default | Purpose |
| --- | --- | --- |
| `dra.driverName` | `thundercompute.com` | DRA driver name; must match `operator.driverName` |
| `deviceClass.name` | `thunder-gpu` | `DeviceClass` workloads reference |
| `node.zoneLabel` | `topology.kubernetes.io/zone` | Node label that maps onto a Thunder zone |
| `node.advertisedIP` | `""` | Pins the advertised IP for every node; empty means per-node resolution |
| `node.advertisedIPLabel` | `thundercompute.com/advertised-ip` | Per-node advertised IP override |
| `operator.validGPUCounts` | `["1"]` | GPU counts a single claim may request |
| `nvidia.minDriverVersion` | `610` | Daemon refuses to enroll older drivers |
| `cdi.specDir` | `/var/run/cdi` | Where the daemon writes CDI specs |
| `dra.kubeletPluginDir` | `/var/lib/kubelet/plugins/thundercompute.com` | Kubelet plugin socket directory |
| `thunder.existingSecret` | `""` | Reuse a pre-created API token secret |

---

## Requesting GPUs

### Pods

Pods should use a `ResourceClaimTemplate`. Kubernetes creates a concrete claim
per pod and deletes it with the pod. The container references the claim through
`resources.claims`, and the daemon returns a claim-scoped CDI device during
prepare.

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
            count: 1
            selectors:
              - cel:
                  expression: device.attributes["thundercompute.com"]["gpu_type"] == "A6000"
            capacity:
              requests:
                thundercompute.com/gpu_count: "1"
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

```bash
kubectl apply -f pod.yaml
kubectl get pod thunder-pod-gpu
kubectl get resourceclaims
```

Or use the test chart:

```bash
helm install thunder-pod charts/tests/pod \
  --set gpu.type=A6000 \
  --set gpu.count=1
```

### Virtual machines

VMs should use a stable `ResourceClaim`, not a `ResourceClaimTemplate`. The
stable claim name is what lets the VM mount the guest artifacts the daemon
creates:

```text
<resourceclaim-name>-thunder-configmap   # install-thunder-client.sh
<resourceclaim-name>-thunder-secret      # enrollment-token
```

Create the claim:

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
          capacity:
            requests:
              thundercompute.com/gpu_count: "1"
```

Reference it from the VM and mount the guest artifacts over virtiofs:

```yaml
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
              runcmd:
                - [ sh, -lc, 'mkdir -p /mnt/thunder-config /mnt/thunder-secret' ]
                - [ sh, -lc, 'for i in $(seq 1 60); do mountpoint -q /mnt/thunder-config || mount -t virtiofs thunder-config /mnt/thunder-config || true; mountpoint -q /mnt/thunder-secret || mount -t virtiofs thunder-secret /mnt/thunder-secret || true; if [ -f /mnt/thunder-config/install-thunder-client.sh ] && [ -s /mnt/thunder-secret/enrollment-token ]; then bash /mnt/thunder-config/install-thunder-client.sh; exit 0; fi; sleep 5; done; echo Thunder guest artifacts unavailable >&2; exit 1' ]
```

The test chart wires up a complete VM with a root disk and networking:

```bash
helm install thunder-vm charts/tests/vm \
  --set gpu.type=A6000 \
  --set gpu.count=1
```

Inspect it:

```bash
kubectl get vm,vmi,pod -l app.kubernetes.io/instance=thunder-vm
kubectl get resourceclaim thunder-vm-thunder-gpu-test-vm-claim -o yaml
kubectl get cm,secret | grep thunder-vm-thunder-gpu-test-vm-claim
```

### ResourceClaim vs ResourceClaimTemplate

Use a `ResourceClaimTemplate` for pods, when Kubernetes should generate and own
one claim per pod. Use a `ResourceClaim` for VMs, when the claim name must be
known before scheduling.

A standalone `ResourceClaim` is safe for VMs: it is not bound to a
`ResourceSlice`, pool, zone, or node until the scheduler processes the
consuming virt-launcher pod.

---

## Development

### Make targets

`make help` prints the full list, grouped by area. The common ones:

| Target | What it does |
| --- | --- |
| `make check` | Everything CI should run: `lint`, `test`, `verify` |
| `make lint` | `gofmt` check plus `go vet` |
| `make test` | Unit tests (`make test-race`, `make cover` for more) |
| `make build` | Both binaries into `bin/`, with version metadata linked in |
| `make image` | Both container images, built from source in a multi-stage build |
| `make helm-lint` | Lint all three charts |
| `make helm-schema` | Check `values.yaml` validates against `values.schema.json` |
| `make verify` | Offline verification — no cluster needed |
| `make preflight` | Read-only cluster readiness checks |
| `make e2e` / `make e2e-vm` | End-to-end tests against a live cluster |
| `make install` / `make uninstall` | Install or remove the chart |
| `make status` / `make logs` | Inspect a running deployment |

Version metadata comes from `git describe` and is linked into both binaries, so
`kubectl logs` reports the exact build. Override it with `make build VERSION=v1.2.3`.

### Building images

Both Dockerfiles are multi-stage and build from source, so an image is
reproducible from a git ref alone:

```bash
make image                              # tagged with the current git version
make image IMAGE_TAG=v1.2.3
make image-buildx PLATFORMS=linux/amd64,linux/arm64
```

The daemon image runs as root and includes `nsenter`, which it needs to drive
the Thunder installer in the host namespaces. The operator image runs as a
non-root user with no host access.

### Testing

Scripts live in [`hack/`](hack/) and are documented in
[`hack/README.md`](hack/README.md). Neither test needs a cluster you already
have, a Thunder account, or a GPU.

```bash
make verify       # offline: build, vet, unit tests, chart renders
make test-local   # creates a throwaway kind cluster and tests against it
```

`make test-local` builds its own Kubernetes cluster with
[kind](https://kind.sigs.k8s.io), installs the chart, runs the **real operator**
against a stub Thunder API, and drives a real `ResourceClaim` through
scheduling — then deletes the cluster. It covers:

- the chart installing against a real API server (CRDs, RBAC, values schema)
- the operator publishing `ResourceSlice`s from Thunder inventory
- the request policy being clamped to what a zone can actually serve
- the scheduler allocating claims from the test charts against those slices
- consumable capacity being honoured, including refusing over-large requests

It cannot cover the node daemon. Preparing a claim needs a real GPU, a Thunder
enrollment and a registered kubelet plugin, so pods stop at `ContainerCreating`
on kind. That path is only exercised on a real Thunder node.

`hack/preflight.sh` (`make preflight`) is not a test — it is a read-only
diagnostic you run against a cluster you are about to deploy to.

---

## Troubleshooting

**Claims stay `Pending` and never allocate.** Check that the operator published
a slice for the GPU type the claim selects:

```bash
kubectl get resourceslices -l app.kubernetes.io/name=thunder-dra-driver -o yaml
kubectl -n thunder-system logs -l app.kubernetes.io/component=operator
```

**Claims allocate but always get one GPU.** The `DRAConsumableCapacity` gate is
off on the API server, so `capacity.requests` was dropped. See
[Verify the cluster](#verify-the-cluster).

**Pods stay `ContainerCreating`.** The kubelet is waiting on prepare. Check the
daemon on the allocated node:

```bash
kubectl -n thunder-system logs -l app.kubernetes.io/component=daemon
kubectl get clients.thundercompute.com -A
```

**The daemon will not start on a node.** It requires a zone and an advertisable
IP. The zone must come from `ZONE` or the zone label; the IP falls back to the
node's own IP, so a failure here means the node has neither an `InternalIP` nor
an `ExternalIP` in `status.addresses`.

**A VM boots without a GPU.** Confirm the guest artifacts exist and that
cloud-init mounted them:

```bash
kubectl get cm,secret | grep <claim-name>-thunder
```

---

## Repository layout

| Path | Contents |
| --- | --- |
| [`cmd/daemon`](cmd/daemon) | Node daemon entrypoint |
| [`cmd/thunder-dra-operator`](cmd/thunder-dra-operator) | Operator entrypoint |
| [`internal/daemon`](internal/daemon) | Node enrollment, DRA kubelet plugin, CDI and guest artifact stores |
| [`internal/operator`](internal/operator) | `ResourceSlice` inventory reconciliation |
| [`internal/version`](internal/version) | Build version and Thunder API user agent |
| [`charts/thunder-device-plugin`](charts/thunder-device-plugin) | Main chart |
| [`charts/tests`](charts/tests) | Pod and VM test charts |
| [`containers`](containers) | Dockerfiles |
| [`hack`](hack) | Verification and end-to-end test scripts |
| [`Makefile`](Makefile) | Build, test, image and deployment targets |

The Thunder API client is the external
[`github.com/Thunder-Compute/thunder-sdk`](https://github.com/Thunder-Compute/thunder-sdk)
module, not a vendored copy.
