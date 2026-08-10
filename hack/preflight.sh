#!/usr/bin/env bash
#
# preflight.sh - checks that a live cluster can run the Thunder DRA driver.
#
# This is a deploy-time diagnostic for a cluster you already have, not part of
# the test suite; `hack/test-local.sh` is the test. Read-only: it creates
# nothing except a server dry-run object that is never persisted.
#
# Verifies the DRA APIs, that consumable capacity survives the API server, node
# labels, and (optionally) the KubeVirt GPUsWithDRA gate.
#
# Usage: hack/preflight.sh [--with-vm] [--namespace NS]

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
source "${REPO_ROOT}/hack/lib/thunder.sh"

WITH_VM=false

while (($#)); do
  case "$1" in
    --with-vm) WITH_VM=true ;;
    --namespace) NAMESPACE="$2"; shift ;;
    -h|--help) usage "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown flag: $1 (try --help)" ;;
  esac
  shift
done

require_cmd kubectl jq

step "Cluster"
if ! kube version -o json >/dev/null 2>&1; then
  die "cannot reach a cluster (check KUBECONFIG / KUBE_CONTEXT)"
fi
check_pass "reachable at $(kube config current-context 2>/dev/null || echo 'current context')"

server_version="$(kube version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // "unknown"')"
log "    server version: ${server_version}"

step "Dynamic Resource Allocation"
for resource in deviceclasses resourceclaims resourceclaimtemplates resourceslices; do
  if api_resource_exists "${resource}"; then
    check_pass "resource.k8s.io serves ${resource}"
  else
    check_fail "resource.k8s.io serves ${resource}" \
      "enable the DynamicResourceAllocation feature gate and the resource.k8s.io/v1 API"
  fi
done

# A server dry run is the only reliable way to tell whether the API server keeps
# consumable capacity requests: without DRAConsumableCapacity it silently drops
# the capacity field instead of rejecting the object. The gate is beta and on by
# default from Kubernetes 1.36, so this should only fail on 1.34-1.35.
capacity_probe="$(cat <<YAML
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: thunder-preflight-capacity-probe
  namespace: default
spec:
  spec:
    devices:
      requests:
        - name: gpu
          exactly:
            deviceClassName: ${DEVICE_CLASS_NAME}
            allocationMode: ExactCount
            count: 1
            capacity:
              requests:
                ${GPU_COUNT_CAPACITY}: "1"
YAML
)"

if probe_output="$(printf '%s' "${capacity_probe}" | kube apply --dry-run=server -o json -f - 2>&1)"; then
  kept="$(printf '%s' "${probe_output}" | jq -r \
    --arg key "${GPU_COUNT_CAPACITY}" \
    '.spec.spec.devices.requests[0].exactly.capacity.requests[$key] // ""')"
  if [[ "${kept}" == "1" ]]; then
    check_pass "DRAConsumableCapacity preserves capacity requests"
  else
    check_fail "DRAConsumableCapacity preserves capacity requests" \
      "the API server dropped ${GPU_COUNT_CAPACITY}. DRAConsumableCapacity is on by default from Kubernetes 1.36; on 1.34-1.35 set feature-gates=DRAConsumableCapacity=true on the API server, scheduler, controller-manager and kubelet"
  fi
else
  check_fail "DRAConsumableCapacity preserves capacity requests" "${probe_output}"
fi

step "Thunder nodes"
nodes="$(thunder_nodes)"
if [[ -z "${nodes}" ]]; then
  check_fail "at least one node is labelled ${THUNDER_NODE_LABEL}=true" \
    "label a GPU node: kubectl label node <node> ${THUNDER_NODE_LABEL}=true"
else
  check_pass "Thunder nodes: ${nodes}"
  for node in ${nodes}; do
    zone="$(kube get node "${node}" -o jsonpath="{.metadata.labels['${ZONE_LABEL//./\\.}']}" 2>/dev/null || true)"
    if [[ -n "${zone}" ]]; then
      check_pass "node/${node} has a zone (${zone})"
    else
      check_fail "node/${node} has a zone" \
        "kubectl label node ${node} ${ZONE_LABEL}=<zone-name>"
    fi

    # The advertised IP is optional: it defaults to the node IP.
    advertised="$(kube get node "${node}" -o jsonpath="{.metadata.labels['${ADVERTISED_IP_LABEL//./\\.}']}" 2>/dev/null || true)"
    if [[ -n "${advertised}" ]]; then
      check_pass "node/${node} advertises ${advertised} (from ${ADVERTISED_IP_LABEL})"
    else
      ip="$(node_ip "${node}")"
      if [[ -n "${ip}" ]]; then
        check_pass "node/${node} advertises ${ip} (node IP default)"
      else
        check_fail "node/${node} has an advertisable IP" \
          "the node has no InternalIP or ExternalIP; set ${ADVERTISED_IP_LABEL} explicitly"
      fi
    fi

    gpu_present="$(kube get node "${node}" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.present}' 2>/dev/null || true)"
    if [[ "${gpu_present}" == "true" ]]; then
      check_pass "node/${node} reports an NVIDIA GPU"
    else
      check_fail "node/${node} reports an NVIDIA GPU" \
        "kubectl label node ${node} nvidia.com/gpu.present=true"
    fi
  done
fi

if "${WITH_VM}"; then
  step "KubeVirt"
  if api_resource_exists virtualmachines; then
    check_pass "KubeVirt is installed"
    gates="$(kube -n kubevirt get kubevirt kubevirt \
      -o jsonpath='{.spec.configuration.developerConfiguration.featureGates}' 2>/dev/null || true)"
    if [[ "${gates}" == *GPUsWithDRA* ]]; then
      check_pass "GPUsWithDRA feature gate is enabled"
    else
      check_fail "GPUsWithDRA feature gate is enabled" \
        "kubectl -n kubevirt patch kubevirt kubevirt --type merge -p '{\"spec\":{\"configuration\":{\"developerConfiguration\":{\"featureGates\":[\"GPUsWithDRA\"]}}}}'"
    fi
  else
    check_fail "KubeVirt is installed" "install KubeVirt or drop --with-vm"
  fi

  if api_resource_exists datavolumes; then
    check_pass "CDI (DataVolume) is installed"
  else
    check_fail "CDI (DataVolume) is installed" "the VM test chart boots from a DataVolume"
  fi
fi

summarize "preflight"
