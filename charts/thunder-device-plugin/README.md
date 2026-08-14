# thunder-device-plugin

Attach [Thunder Compute](https://www.thundercompute.com) GPUs to Kubernetes pods
and KubeVirt VMs through Dynamic Resource Allocation (DRA).

Workloads request a GPU either as an extended resource named after the model
(`thundercompute.com/gpu-a6000: 2`) or through a `ResourceClaim` naming the
model's `DeviceClass`. Either way the GPUs come from the zone pool, not from
node-local `allocatable`.

The chart deploys two components:

- **operator** — a Deployment that reads Thunder inventory, publishes one
  `ResourceSlice` pool per zone and GPU type (one device per GPU, scaled by the
  zone's oversubscription target from the Thunder API), and generates a
  `DeviceClass` per GPU type so each model has its own extended resource.
- **daemon** — a DaemonSet on Thunder GPU nodes that enrolls the node and serves
  the DRA kubelet plugin.

Full setup instructions, including the cluster feature gates this chart
requires, are in the [repository README](../../README.md).

## TL;DR

```bash
kubectl create namespace thunder-system
kubectl -n thunder-system create secret generic thunder-api \
  --from-literal=THUNDER_API_TOKEN='<token>'

helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system --create-namespace

helm test thunder-device-plugin --namespace thunder-system
```

## Requirements

| Requirement | Notes |
| --- | --- |
| Kubernetes `>=1.34.0` | Enforced by `kubeVersion`; `resource.k8s.io/v1` must be served |
| `DRAExtendedResource` | Only for the `resources.limits` form. Beta and on by default from 1.36. `ResourceClaim`s need no gate |
| A Thunder API token | Needs zones, nodes, clients and enrollment-token permissions |
| Node labels | `thundercompute.com/node=true` and a zone label, on GPU-serving nodes only |

## Installing

The chart never stores the Thunder API token: create a Secret holding it first,
and point the chart at it. It goes into the release namespace.

```bash
kubectl -n thunder-system create secret generic thunder-api \
  --from-literal=THUNDER_API_TOKEN='<token>'

helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system --create-namespace
```

Point at a differently named Secret with `--set thunder.secretName=<name>`. It
must hold the token under the key `THUNDER_API_TOKEN`.

### CRDs

The chart ships `clients.thundercompute.com` in `crds/`. Helm installs CRDs on
first install but never upgrades them, so apply them explicitly when upgrading:

```bash
kubectl apply -f charts/thunder-device-plugin/crds/
```

### Uninstalling

```bash
helm uninstall thunder-device-plugin --namespace thunder-system
```

Helm does not remove CRDs. Delete them by hand if you want the
`ThunderClient` resources gone as well.

## Testing the release

```bash
helm test thunder-device-plugin --namespace thunder-system
```

The test polls for `ResourceSlice`s published by the operator, which is the
end-to-end signal that the chart reached the Thunder API and produced usable
inventory. Disable it with `--set tests.enabled=false`.

## Values

Values are validated against [`values.schema.json`](values.schema.json), so
typos and wrong types fail at install time rather than silently doing nothing.

### Deployment

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `driverName` | string | `thundercompute.com` | DRA driver name. Identifies the driver to the scheduler and kubelet and names its CDI devices |
| `imagePullSecrets` | list | `[]` | Pull secrets for both components |
| `nameOverride` | string | `""` | Override the chart name used in resource names |
| `fullnameOverride` | string | `""` | Override the generated resource names |

### Thunder API

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `thunder.secretName` | string | `thunder-api` | Existing Secret holding the API token under `THUNDER_API_TOKEN`. Must exist before install |
| `thunder.apiURL` | string | `https://registry.thundercompute.com` | Thunder API endpoint |
| `thunder.artifactBaseURL` | string | `https://get.thundercompute.com` | Host the Thunder client artifacts are downloaded from. The daemon fetches `libthunder.so` from here and stages it into every container that claims a GPU. Point at a staging artifact host to run unreleased client builds. |
| `thunder.installURL` | string | `""` | Installer the daemon reads the pinned libthunder.so digest out of. It is read, never executed. Empty derives it as `<artifactBaseURL>/install.sh`. |
| `thunder.telemetryURL` | string | `https://telemetry.thundercompute.com:2096` | Telemetry collector written into each container's Thunder client config. |

### Operator

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `operator.enabled` | bool | `true` | Deploy the operator. Without it no GPUs are advertised |
| `operator.image.repository` | string | `ghcr.io/thunder-compute/thunder-device-plugin/operator` | Operator image |
| `operator.image.tag` | string | `""` | Operator image tag; release commits record the tag published by `make publish-images` |
| `operator.image.pullPolicy` | string | `IfNotPresent` | Operator image pull policy |
| `operator.reconcileInterval` | string | `60s` | How often Thunder inventory is polled |
| `operator.orphanGracePeriod` | string | `5m` | How long a per-claim client resource may outlive its `ResourceClaim` before its Thunder enrollment is revoked and the resource removed |
| `operator.extendedResourcePrefix` | string | `thundercompute.com/gpu-` | Prefix for the extended resource of each GPU model. `""` publishes classes without them, for clusters below 1.36 |
| `operator.podAnnotations` | object | `{}` | Annotations for operator pods |
| `operator.podLabels` | object | `{}` | Labels for operator pods |
| `operator.nodeSelector` | object | `{}` | Node selector for operator pods |
| `operator.tolerations` | list | `[]` | Tolerations for operator pods |
| `operator.affinity` | object | `{}` | Affinity for operator pods |
| `operator.resources` | object | `{}` | Resources for the operator container |

### Node daemon

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `daemon.image.repository` | string | `ghcr.io/thunder-compute/thunder-device-plugin/daemon` | Daemon image |
| `daemon.image.tag` | string | `""` | Daemon image tag; release commits record the tag published by `make publish-images` |
| `daemon.image.pullPolicy` | string | `IfNotPresent` | Daemon image pull policy |
| `daemon.podAnnotations` | object | `{}` | Annotations for daemon pods |
| `daemon.podLabels` | object | `{}` | Labels for daemon pods |
| `daemon.nodeSelector` | object | `{}` | Additional node selector for daemon pods |
| `daemon.tolerations` | list | `[]` | Tolerations for daemon pods |
| `daemon.affinity` | object | `{}` | Additional affinity for daemon pods |
| `daemon.resources` | object | `{}` | Resources for the daemon container |

### Node labels

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `node.thunderLabel` | string | `thundercompute.com/node` | Label marking a node Thunder-eligible |
| `node.zoneLabel` | string | `topology.kubernetes.io/zone` | Label carrying the Thunder zone |
| `node.advertisedIPLabel` | string | `thundercompute.com/advertised-ip` | Per-node override for the address clients use |

The advertised IP defaults to the node's own IP (`status.addresses` `InternalIP`,
then `ExternalIP`). Set the label only when clients reach a node on a different
address, for example behind NAT.

### NVIDIA, kubelet and runtime

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `nvidia.minDriverVersion` | string | `610` | Minimum driver version the daemon will enroll |
| `nvidia.libcudaPath` | string | `/usr/lib/x86_64-linux-gnu/libcuda.so.1` | Host `libcuda`, bind-mounted into containers |
| `nvidia.libnvidiaMLPath` | string | `/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1` | Host NVML library |
| `nvidia.nvidiaSMIPath` | string | `/usr/bin/nvidia-smi` | Host `nvidia-smi` |
| `kubelet.pluginDirRoot` | string | `/var/lib/kubelet/plugins` | Parent directory for plugin sockets. Change it on distributions that move the kubelet root |
| `kubelet.registrarDir` | string | `/var/lib/kubelet/plugins_registry` | Directory the kubelet watches for plugin registration |
| `cdi.specDir` | string | `/var/run/cdi` | Where the daemon writes CDI specs |

### Tests

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `tests.enabled` | bool | `true` | Render the `helm test` hook |
| `tests.image.repository` | string | `registry.k8s.io/kubectl` | Image used by `helm test` |
| `tests.image.tag` | string | `v1.34.0` | Test image tag |
| `tests.image.pullPolicy` | string | `IfNotPresent` | Test image pull policy |
| `tests.timeoutSeconds` | int | `120` | How long the test waits for inventory |

## Leaked enrollments

Each prepared claim gets a `clients.thundercompute.com` resource recording the
Thunder enrollment behind it. It carries a finalizer, so deleting it — or the
namespace holding it — cannot quietly drop the only record of a live enrollment:
the daemon's cleanup treats a missing resource as nothing to do, which would
leave the enrollment consuming zone capacity forever.

The daemon releases the finalizer during normal teardown. When it cannot, for
example because the node is gone, the operator revokes the enrollment and
removes the resource once its `ResourceClaim` has been absent for
`operator.orphanGracePeriod`.

```bash
kubectl get clients.thundercompute.com -A
```

## Security

The daemon runs **privileged** with `hostPID: true`. It needs this to enter the
host namespaces (via `nsenter`) to install and manage the Thunder client, to
write CDI specs the container runtime reads, and to serve the kubelet plugin
socket. The operator runs unprivileged as a non-root user and only talks to the
Kubernetes and Thunder APIs.

Both components read the Thunder API token from a Secret you create yourself.
The chart never takes the token as a value, so it is never written to the release
history.

## Source

- Chart and source: <https://github.com/Thunder-Compute/thunder-device-plugin>
- Thunder SDK: <https://github.com/Thunder-Compute/thunder-sdk>
