# thunder-device-plugin

Attach [Thunder Compute](https://www.thundercompute.com) GPUs to Kubernetes pods
and KubeVirt VMs through Dynamic Resource Allocation (DRA).

Workloads request a GPU either as an extended resource named after the model
(`thundercompute.com/gpu-a6000: 2`) or through a `ResourceClaim` with a CEL
selector. Either way the GPUs come from the zone pool, not from node-local
`allocatable`.

The chart deploys two components:

- **operator** — a Deployment that reads Thunder inventory, publishes one
  `ResourceSlice` pool per zone and GPU type (one device per GPU), and generates
  a `DeviceClass` per GPU type so each model has its own extended resource.
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
  --namespace thunder-system \
  --set namespace.create=false \
  --set thunder.existingSecret=thunder-api

helm test thunder-device-plugin --namespace thunder-system
```

## Requirements

| Requirement | Notes |
| --- | --- |
| Kubernetes `>=1.34.0` | Enforced by `kubeVersion`; `resource.k8s.io/v1` must be served |
| `DRAExtendedResource` | Only for the `resources.limits: thundercompute.com/gpu` form. Beta and on by default from 1.36 |
| `DRAConsumableCapacity` | Only when `operator.sharesPerGPU > 1`. Beta and on by default from 1.36 |
| A Thunder API token | Needs zones, nodes, clients and enrollment-token permissions |
| Node labels | `thundercompute.com/node=true`, a zone label, `nvidia.com/gpu.present=true` |

## Installing

The chart needs a Thunder API token. Prefer a pre-created Secret so the token
does not end up in the Helm release history:

```bash
helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system --create-namespace \
  --set thunder.existingSecret=thunder-api
```

To let the chart create the Secret instead:

```bash
helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system --create-namespace \
  --set-file thunder.apiToken=/path/to/token
```

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
| `namespace.create` | bool | `true` | Create the namespace as part of the release |
| `namespace.name` | string | `thunder-system` | Namespace both components run in |
| `image.repository` | string | `thundercompute/thunder-device-plugin-daemon` | Daemon image |
| `image.tag` | string | `latest` | Daemon image tag |
| `image.pullPolicy` | string | `IfNotPresent` | Daemon image pull policy |
| `imagePullSecrets` | list | `[]` | Pull secrets for both components |
| `nameOverride` | string | `""` | Override the chart name |
| `fullnameOverride` | string | `""` | Override the generated resource names |

### Operator

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `operator.enabled` | bool | `true` | Deploy the inventory operator |
| `operator.replicas` | int | `1` | Operator replica count |
| `operator.image.repository` | string | `thundercompute/thunder-dra-operator` | Operator image |
| `operator.image.tag` | string | `latest` | Operator image tag |
| `operator.image.pullPolicy` | string | `IfNotPresent` | Operator image pull policy |
| `operator.driverName` | string | `thundercompute.com` | DRA driver name; must match `dra.driverName` |
| `operator.resourceSliceNamePrefix` | string | `thunder` | Prefix for generated `ResourceSlice` names |
| `operator.reconcileInterval` | string | `60s` | Go duration between inventory reconciles |
| `operator.sharesPerGPU` | int | `1` | Clients allowed to share one GPU. `1` keeps GPUs exclusive and publishes no consumable capacity; above `1` needs Kubernetes 1.36+ |
| `operator.deviceClassPrefix` | string | `thunder-gpu-` | Name prefix for the generated per-GPU-type `DeviceClass`es |
| `operator.extendedResourcePrefix` | string | `thundercompute.com/gpu-` | Prefix for per-GPU-type extended resources, so `resources.limits: thundercompute.com/gpu-a6000: 2` pins the model. `""` disables the generated classes |
| `operator.podAnnotations` | object | `{}` | Extra operator pod annotations |
| `operator.podLabels` | object | `{}` | Extra operator pod labels |
| `operator.nodeSelector` | object | `{}` | Operator node selector |
| `operator.tolerations` | list | `[]` | Operator tolerations |
| `operator.affinity` | object | `{}` | Operator affinity |
| `operator.resources` | object | `{}` | Operator resource requests and limits |

### Thunder API

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `thunder.apiToken` | string | `""` | API token. Required unless `existingSecret` is set |
| `thunder.apiURL` | string | `https://api.thundercompute.com:2096` | Thunder API base URL |
| `thunder.existingSecret` | string | `""` | Reuse a pre-created Secret instead of creating one |
| `thunder.secretName` | string | `thunder-api` | Name of the Secret the chart creates |
| `thunder.apiTokenKey` | string | `THUNDER_API_TOKEN` | Secret key holding the token |
| `thunder.apiURLKey` | string | `THUNDER_API_URL` | Secret key holding the API URL |

### Node identity

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `node.zone` | string | `""` | Pins the Thunder zone for every node; empty resolves per node from `zoneLabel` |
| `node.advertisedIP` | string | `""` | Pins the advertised IP for every node; empty resolves per node |
| `node.zoneLabel` | string | `topology.kubernetes.io/zone` | Node label carrying the zone |
| `node.advertisedIPLabel` | string | `thundercompute.com/advertised-ip` | Per-node advertised IP override |

The advertised IP is the address Thunder clients use to reach a node. It
resolves in this order: `node.advertisedIP`, then the `node.advertisedIPLabel`
label, then the node's own IP (`status.addresses` `InternalIP`, then
`ExternalIP`). Most clusters need no configuration here.

### DRA and CDI

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `dra.enabled` | bool | `true` | Serve the DRA kubelet plugin |
| `dra.driverName` | string | `thundercompute.com` | DRA driver name; must match `operator.driverName` |
| `dra.thunderClientNamespace` | string | `thunder-system` | Namespace for `ThunderClient` resources |
| `dra.kubeletPluginDir` | string | `/var/lib/kubelet/plugins/thundercompute.com` | Kubelet plugin socket directory |
| `dra.kubeletRegistrarDir` | string | `/var/lib/kubelet/plugins_registry` | Kubelet plugin registrar directory |
| `deviceClass.enabled` | bool | `true` | Create the `DeviceClass` |
| `deviceClass.name` | string | `thunder-gpu` | `DeviceClass` name workloads reference |
| `deviceClass.extendedResourceName` | string | `""` | Catch-all extended resource for the class. Empty by default: this class matches every GPU model, and a `DeviceClass` cannot require one request to stay within a single model. Only set it if every zone serves one GPU type |
| `cdi.specDir` | string | `/var/run/cdi` | Where the daemon writes CDI specs |

### NVIDIA and host access

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `nvidia.minDriverVersion` | string | `610` | Minimum driver version the daemon will enroll |
| `nvidia.libcudaPath` | string | `/usr/lib/x86_64-linux-gnu/libcuda.so.1` | Host `libcuda` bind-mounted into containers |
| `nvidia.libnvidiaMLPath` | string | `/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1` | Host NVML library |
| `nvidia.nvidiaSMIPath` | string | `/usr/bin/nvidia-smi` | Host `nvidia-smi` |
| `hostRoot` | string | `/host` | Where the host filesystem is mounted in the daemon |
| `hostTargetPID` | string | `"1"` | Host PID whose namespaces the daemon enters; `0` disables `nsenter` |

### Scheduling

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `nodeSelector` | object | see [values.yaml](values.yaml) | Daemon node selector |
| `nodeLabelKeys.thunderNode` | string | `thundercompute.com/node` | Label marking a node Thunder-eligible |
| `tolerations` | list | `[]` | Daemon tolerations |
| `affinity` | object | `{}` | Extra daemon affinity, merged with the required node affinity |
| `podAnnotations` | object | `{}` | Extra daemon pod annotations |
| `podLabels` | object | `{}` | Extra daemon pod labels |
| `resources` | object | `{}` | Daemon resource requests and limits |

### Tests

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `tests.enabled` | bool | `true` | Render the `helm test` hook |
| `tests.image.repository` | string | `registry.k8s.io/kubectl` | Image used by `helm test` |
| `tests.image.tag` | string | `v1.34.0` | Test image tag |
| `tests.image.pullPolicy` | string | `IfNotPresent` | Test image pull policy |
| `tests.timeoutSeconds` | int | `120` | How long the test waits for inventory |

## Security

The daemon runs **privileged** with `hostPID: true`. It needs this to enter the
host namespaces (via `nsenter`) to install and manage the Thunder client, to
write CDI specs the container runtime reads, and to serve the kubelet plugin
socket. The operator runs unprivileged as a non-root user and only talks to the
Kubernetes and Thunder APIs.

Both components read the Thunder API token from a Secret. When the chart creates
that Secret from `thunder.apiToken`, the token is also stored in the Helm
release history; use `thunder.existingSecret` in production.

## Source

- Chart and source: <https://github.com/Thunder-Compute/thunder-device-plugin>
- Thunder SDK: <https://github.com/Thunder-Compute/thunder-sdk>
