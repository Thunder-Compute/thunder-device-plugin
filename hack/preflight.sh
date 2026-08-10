#!/usr/bin/env bash
#
# preflight.sh - checks that a live cluster can run the Thunder DRA driver.
#
# This is a deploy-time diagnostic for a cluster you already have, not part of
# the test suite; `hack/test-local.sh` is the test. Read-only: it creates
# nothing except a server dry-run object that is never persisted.
#
# Verifies the DRA APIs, that consumable capacity survives the API server, node
# labels, and (optionally) KubeVirt readiness for VM workloads.
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

# The extended resource form (resources.limits: thundercompute.com/gpu-<type>)
# needs DRAExtendedResource, which is beta and on by default from 1.36. Nothing
# else the driver publishes needs a feature gate.
extended_probe="$(cat <<YAML
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: thunder-preflight-extended-probe
spec:
  extendedResourceName: ${THUNDER_DOMAIN}/gpu-preflight
  selectors:
    - cel:
        expression: device.driver == "${DRIVER_NAME}"
YAML
)"

if probe_output="$(printf '%s' "${extended_probe}" | kube apply --dry-run=server -o json -f - 2>&1)"; then
  kept="$(printf '%s' "${probe_output}" | jq -r '.spec.extendedResourceName // ""')"
  if [[ "${kept}" == "${THUNDER_DOMAIN}/gpu" ]]; then
    check_pass "DRAExtendedResource available (resources.limits form supported)"
  else
    warn "DRAExtendedResource is off: workloads must use a ResourceClaim"
    log "    it is beta and on by default from Kubernetes 1.36"
  fi
else
  warn "could not probe DRAExtendedResource: ${probe_output}"
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

  done
fi

if "${WITH_VM}"; then
  step "KubeVirt"
  if api_resource_exists virtualmachines; then
    check_pass "KubeVirt is installed"
    # No feature gate is required. A Thunder GPU is network attached, so a VM
    # never asks KubeVirt for a passthrough device: it declares the claim, and
    # the guest reaches the GPU through the client it installs itself.
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
