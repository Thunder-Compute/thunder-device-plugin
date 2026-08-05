# Thunder Device Plugin

Thunder Device Plugin exposes Thunder Compute virtual GPUs through Kubernetes
Dynamic Resource Allocation. The chart installs a cluster operator for
ResourceSlice inventory and a node daemon that prepares allocated claims on the
selected node.

## Architecture

The operator publishes one fungible `ResourceSlice` per zone and GPU type. The
advertised capacity is the larger of healthy host GPUs and currently committed
Thunder clients, so active allocations remain represented even if backing
capacity changes.

The daemonset runs on Thunder GPU nodes. It enrolls the node, serves the DRA
kubelet plugin, mints one Thunder client enrollment token per prepared
`ResourceClaim`, writes pod CDI specs, and creates per-claim VM guest artifacts.

```text
ResourceSlice -> scheduler allocation -> ResourceClaim status -> kubelet prepare -> ThunderClient + CDI/guest artifacts
```

## Kubernetes Setup

Enable consumable DRA capacity on the API server, scheduler,
controller-manager, and kubelet.

RKE2 server config at `/etc/rancher/rke2/config.yaml`:

```yaml
kube-apiserver-arg:
  - "feature-gates=DRAConsumableCapacity=true"
kube-scheduler-arg:
  - "feature-gates=DRAConsumableCapacity=true"
kube-controller-manager-arg:
  - "feature-gates=DRAConsumableCapacity=true"
```

RKE2 worker config at `/etc/rancher/rke2/config.yaml`:

```yaml
kubelet-arg:
  - "feature-gates=DRAConsumableCapacity=true"
```

Restart RKE2:

```bash
sudo systemctl restart rke2-server
sudo systemctl restart rke2-agent
```

Verify the API server preserves DRA capacity requests:

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
            deviceClassName: thunder-vgpu
            allocationMode: ExactCount
            count: 1
            selectors:
              - cel:
                  expression: device.attributes["vgpu.thundercompute.com"]["gpu_type"] == "A6000"
            capacity:
              requests:
                vgpu.thundercompute.com/gpu_count: "1"
EOF
```

Verify Thunder slices use shared consumable capacity:

```bash
kubectl get resourceslices -l app.kubernetes.io/name=thunder-dra-driver -o yaml \
  | grep -A12 allowMultipleAllocations
```

## KubeVirt Setup

KubeVirt still needs `GPUsWithDRA` because VMs reference DRA claims through the
VMI spec.

Enable the feature gate:

```bash
kubectl -n kubevirt patch kubevirt kubevirt \
  --type merge \
  -p '{"spec":{"configuration":{"developerConfiguration":{"featureGates":["GPUsWithDRA"]}}}}'
```

Verify the feature gate:

```bash
kubectl -n kubevirt get kubevirt kubevirt \
  -o jsonpath='{.spec.configuration.developerConfiguration.featureGates}{"\n"}'
```

Expected output includes:

```text
["GPUsWithDRA"]
```

## Node Labels Setup

The daemonset schedules only onto Thunder GPU nodes. Each eligible node needs a
zone, an externally reachable IP label, and the Thunder node labels expected by
the chart.

Required labels:

```bash
kubectl label node <node-name> thundercompute.com/node=true
kubectl label node <node-name> topology.kubernetes.io/zone=<zone-name>
kubectl label node <node-name> thundercompute.com/external-ip=<node-ip>
kubectl label node <node-name> nvidia.com/gpu.present=true
```

Optional inventory/debug labels:

```bash
kubectl label node <node-name> thundercompute.com/gpu-count=<count>
kubectl label node <node-name> thundercompute.com/gpu-driver-version=<version>
kubectl label node <node-name> nvidia.com/gpu.product=<gpu-type>
```

Verify node labels:

```bash
kubectl get nodes --show-labels | grep thundercompute.com/node
```

## Chart Deployment

Create or reuse a secret containing a Thunder API token. The token must have the
Thunder API permissions needed for zones, nodes, clients, and enrollment tokens.

Create the namespace and secret:

```bash
kubectl create namespace thunder-system
kubectl -n thunder-system create secret generic thunder-api \
  --from-literal=THUNDER_API_TOKEN='<token>' \
  --from-literal=THUNDER_API_URL='https://api.thundercompute.com:2096'
```

Install or upgrade with an existing secret:

```bash
helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system \
  --set namespace.create=false \
  --set namespace.name=thunder-system \
  --set thunder.existingSecret=thunder-api
```

Install or upgrade with a direct token value:

```bash
helm upgrade --install thunder-device-plugin charts/thunder-device-plugin \
  --namespace thunder-system \
  --create-namespace \
  --set thunder.apiToken='<token>'
```

Apply the CRD explicitly when upgrading an existing release. Helm does not
upgrade files in `crds/` during normal chart upgrades.

```bash
kubectl apply -f charts/thunder-device-plugin/crds/clients.thundercompute.com.yaml
```

Verify rollout:

```bash
kubectl -n thunder-system rollout status deployment/thunder-device-plugin-operator
kubectl -n thunder-system rollout status daemonset/thunder-device-plugin
kubectl get deviceclasses thunder-vgpu
kubectl get resourceslices -l app.kubernetes.io/name=thunder-dra-driver
```

## Pod Requests

Pods should use `ResourceClaimTemplate`. Kubernetes creates a concrete claim for
each pod and deletes it with the pod. The container requests the claim through
`resources.claims`, and the daemon returns a claim-scoped CDI device during
prepare.

Pod request example:

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
            deviceClassName: thunder-vgpu
            allocationMode: ExactCount
            count: 1
            selectors:
              - cel:
                  expression: device.attributes["vgpu.thundercompute.com"]["gpu_type"] == "A6000"
            capacity:
              requests:
                vgpu.thundercompute.com/gpu_count: "1"
---
apiVersion: v1
kind: Pod
metadata:
  name: thunder-pod-gpu
spec:
  restartPolicy: Never
  resourceClaims:
    - name: vgpu
      resourceClaimTemplateName: thunder-pod-gpu
  containers:
    - name: tester
      image: ubuntu:24.04
      command: ["bash", "-lc", "nvidia-smi && sleep 3600"]
      resources:
        claims:
          - name: vgpu
            request: gpu
```

Apply and inspect:

```bash
kubectl apply -f pod.yaml
kubectl get pod thunder-pod-gpu
kubectl get resourceclaims
```

## VM Requests

VMs should use a stable `ResourceClaim`, not `ResourceClaimTemplate`. The stable
claim name lets the VM mount the daemon-created guest ConfigMap and Secret:

```text
<resourceclaim-name>-thunder-configmap
<resourceclaim-name>-thunder-secret
```

Create the VM claim:

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
          deviceClassName: thunder-vgpu
          allocationMode: ExactCount
          count: 1
          selectors:
            - cel:
                expression: device.attributes["vgpu.thundercompute.com"]["gpu_type"] == "A6000"
          capacity:
            requests:
              vgpu.thundercompute.com/gpu_count: "1"
```

Reference the claim from the VM and mount the Thunder guest artifacts with
virtiofs:

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
        - name: vgpu
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

Use the test chart for a complete VM with root disk and networking:

```bash
helm install thunder-vm charts/tests/vm \
  --set gpu.type=A6000 \
  --set gpu.count=1
```

Inspect the VM and its generated Thunder artifacts:

```bash
kubectl get vm,vmi,pod -l app.kubernetes.io/instance=thunder-vm
kubectl get resourceclaim thunder-vm-thunder-vgpu-test-vm-claim -o yaml
kubectl get cm,secret | grep thunder-vm-thunder-vgpu-test-vm-claim
```

## ResourceClaim vs ResourceClaimTemplate

Use `ResourceClaimTemplate` for pods when Kubernetes should generate and own one
claim per pod. Use `ResourceClaim` for VMs when the claim name must be known
before scheduling.

A standalone `ResourceClaim` is safe for VMs: it is not bound to a
`ResourceSlice`, pool, zone, or node until the scheduler processes the consuming
virt-launcher pod.
