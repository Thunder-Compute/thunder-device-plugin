#!/usr/bin/env bash
# Project-specific constants and helpers. Source after lib/common.sh.

if [[ -n "${_THUNDER_SH:-}" ]]; then
  return 0
fi
_THUNDER_SH=1

# Chart locations.
CHART_MAIN="${REPO_ROOT}/charts/thunder-device-plugin"
CHART_TEST_POD="${REPO_ROOT}/charts/tests/pod"
CHART_TEST_VM="${REPO_ROOT}/charts/tests/vm"

# Resource identity. Everything Thunder publishes lives under this domain.
THUNDER_DOMAIN="thundercompute.com"
DRIVER_NAME="${DRIVER_NAME:-${THUNDER_DOMAIN}}"
DEVICE_CLASS_NAME="${DEVICE_CLASS_NAME:-thunder-gpu}"
GPU_COUNT_CAPACITY="${THUNDER_DOMAIN}/gpu_count"
GPU_TYPE_ATTRIBUTE="gpu_type"
GPU_TYPE_ATTRIBUTE_KEY="${THUNDER_DOMAIN}/${GPU_TYPE_ATTRIBUTE}"
ADVERTISED_IP_LABEL="${THUNDER_DOMAIN}/advertised-ip"
THUNDER_NODE_LABEL="${THUNDER_DOMAIN}/node"
ZONE_LABEL="${ZONE_LABEL:-topology.kubernetes.io/zone}"
DRIVER_APP_LABEL="app.kubernetes.io/name=thunder-dra-driver"

# Deployment defaults, all overridable from the environment.
NAMESPACE="${NAMESPACE:-thunder-system}"
RELEASE="${RELEASE:-thunder-device-plugin}"
GPU_TYPE="${GPU_TYPE:-A6000}"
GPU_COUNT="${GPU_COUNT:-1}"
TIMEOUT="${TIMEOUT:-600}"
POLL_INTERVAL="${POLL_INTERVAL:-5}"

# thunder_client_name <claim-uid> mirrors daemon.ThunderClientName.
thunder_client_name() {
  local uid="$1"
  printf 'claim-%s' "$(printf '%s' "${uid}" | tr '[:upper:]_' '[:lower:]-')"
}

# guest_configmap_name <claim-name> mirrors daemon.ThunderGuestConfigMapName.
guest_configmap_name() {
  printf '%s-thunder-configmap' "$1"
}

# guest_secret_name <claim-name> mirrors daemon.ThunderGuestSecretName.
guest_secret_name() {
  printf '%s-thunder-secret' "$1"
}

# claim_by_label <namespace> <label-selector> prints the first matching claim name.
claim_by_label() {
  kube -n "$1" get resourceclaims -l "$2" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

# claim_field <namespace> <name> <jsonpath> prints a ResourceClaim field.
claim_field() {
  kube -n "$1" get resourceclaim "$2" -o jsonpath="$3" 2>/dev/null || true
}

# claim_is_allocated <namespace> <name>
claim_is_allocated() {
  [[ -n "$(claim_field "$1" "$2" '{.status.allocation}')" ]]
}

# claim_exists_for_label <namespace> <label-selector>
claim_exists_for_label() {
  [[ -n "$(claim_by_label "$1" "$2")" ]]
}

# pod_is_running <namespace> <name>
pod_is_running() {
  [[ "$(kube -n "$1" get pod "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]]
}

# vmi_is_running <namespace> <name>
vmi_is_running() {
  [[ "$(kube -n "$1" get vmi "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]]
}

# resource_gone <args>... is true once kubectl can no longer get the resource.
resource_gone() {
  ! kube get "$@" >/dev/null 2>&1
}

# resources_gone <namespace> <type/name>... is true once none of them exist.
resources_gone() {
  local namespace="$1"
  shift
  local target
  for target in "$@"; do
    kube -n "${namespace}" get "${target}" >/dev/null 2>&1 && return 1
  done
  return 0
}

# workload_name <release> <chart> prints the fullname the test charts generate.
workload_name() {
  printf '%s-%s' "$1" "$2"
}

# chart_workload <namespace> <component> <kind> prints the driver workload name.
chart_workload() {
  kube -n "$1" get "$3" -l "app.kubernetes.io/component=$2" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

# driver_slices_exist is true once the operator published inventory.
driver_slices_exist() {
  local count
  count="$(kube get resourceslices -l "${DRIVER_APP_LABEL}" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w)"
  ((count > 0))
}

# slice_for_gpu_type <gpu-type> prints the slice advertising that GPU type.
slice_for_gpu_type() {
  local gpu_type="$1"
  kube get resourceslices -l "${DRIVER_APP_LABEL}" -o json 2>/dev/null | jq -r \
    --arg want "${gpu_type}" --arg attr "${GPU_TYPE_ATTRIBUTE_KEY}" '
      .items[]
      | select(any(.spec.devices[]?.attributes[$attr]?.string // ""; ascii_downcase == ($want | ascii_downcase)))
      | .metadata.name' | head -1
}

# thunder_nodes prints the nodes the DaemonSet targets.
thunder_nodes() {
  kube get nodes -l "${THUNDER_NODE_LABEL}=true" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true
}

# node_ip <node> prints the node's InternalIP, falling back to its ExternalIP.
node_ip() {
  local node="$1" ip
  ip="$(kube get node "${node}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | awk '{print $1}')"
  if [[ -z "${ip}" ]]; then
    ip="$(kube get node "${node}" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null | awk '{print $1}')"
  fi
  printf '%s' "${ip}"
}
