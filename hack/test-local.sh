#!/usr/bin/env bash
#
# test-local.sh - integration test on a throwaway local kind cluster.
#
# Creates its own cluster, installs the chart, runs the real operator against a
# stub Thunder API, and drives a real ResourceClaim through scheduling. Nothing
# is deployed to any existing cluster and no Thunder account, API token or GPU
# is required.
#
# What this covers:
#   - the chart installs against a real API server (CRDs, RBAC, values schema)
#   - the real operator publishes ResourceSlices from Thunder inventory
#   - the request policy is clamped to what a zone can actually serve
#   - the scheduler allocates claims from the test charts against those slices
#   - consumable capacity is honoured, including refusing over-large requests
#
# What this cannot cover: the node daemon. Preparing a claim needs a real GPU,
# a Thunder enrollment and a registered kubelet plugin, so pods stop at
# ContainerCreating here. That path is only exercised on a real Thunder node.
#
# Usage: hack/test-local.sh [--keep] [--reuse] [--node-image IMAGE]

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
source "${REPO_ROOT}/hack/lib/thunder.sh"

# This test defines its own synthetic inventory, so it deliberately does not
# inherit GPU_TYPE/ZONE/NAMESPACE from the environment the way the operator
# facing scripts do: an unrelated ambient value would silently desync the stub
# inventory from the claims asserted against it.
GPU_TYPE="A6000"
ZONE="local-zone"
NAMESPACE="thunder-system"
RELEASE="thunder-device-plugin"
GPU_CAPACITY=4

CLUSTER="${CLUSTER:-thunder-local}"
# 1.36 is the first release where DRAConsumableCapacity is beta and on by
# default. The kind config below enables it explicitly so older images work too.
NODE_IMAGE="${NODE_IMAGE:-kindest/node:v1.36.1}"
STUB_PORT="${STUB_PORT:-0}"
KEEP=false
REUSE=false

KIND="${KIND:-kind}"
WORK_DIR=""
STUB_PID=""
OPERATOR_PID=""

while (($#)); do
  case "$1" in
    --keep) KEEP=true ;;
    --reuse) REUSE=true ;;
    --node-image) NODE_IMAGE="$2"; shift ;;
    --cluster) CLUSTER="$2"; shift ;;
    -h|--help) usage "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown flag: $1 (try --help)" ;;
  esac
  shift
done

require_cmd docker kubectl helm go jq python3 curl
if ! command -v "${KIND}" >/dev/null 2>&1; then
  die "kind is required: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
fi

WORK_DIR="$(mktemp -d)"
export KUBECONFIG="${WORK_DIR}/kubeconfig"

cleanup() {
  local status=$?
  set +e
  [[ -n "${OPERATOR_PID}" ]] && kill "${OPERATOR_PID}" 2>/dev/null
  [[ -n "${STUB_PID}" ]] && kill "${STUB_PID}" 2>/dev/null

  if ((status != 0)) || ((CHECKS_FAILED > 0)); then
    step "Diagnostics"
    dump_section "operator log" -- tail -40 "${WORK_DIR}/operator.log"
    dump_section "resourceslices" -- kube get resourceslices -o wide
    dump_section "resourceclaims" -- kube get resourceclaims -A -o wide
    dump_section "pods" -- kube get pods -A
  fi

  if "${KEEP}"; then
    step "Keeping cluster ${CLUSTER} (--keep)"
    log "    export KUBECONFIG=${WORK_DIR}/kubeconfig"
    log "    ${KIND} delete cluster --name ${CLUSTER}"
    return
  fi
  step "Cleanup"
  "${KIND}" delete cluster --name "${CLUSTER}" >/dev/null 2>&1
  rm -rf "${WORK_DIR}"
  log "    done"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------

create_cluster() {
  step "Local cluster"
  if "${REUSE}" && "${KIND}" get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    check_pass "reusing kind cluster ${CLUSTER}"
  else
    "${KIND}" delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
    cat > "${WORK_DIR}/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
# Beta and on by default from 1.36; set explicitly so older node images work.
featureGates:
  DRAConsumableCapacity: true
nodes:
  - role: control-plane
EOF
    log "    creating kind cluster ${CLUSTER} (${NODE_IMAGE})"
    if ! "${KIND}" create cluster --name "${CLUSTER}" --image "${NODE_IMAGE}" \
      --config "${WORK_DIR}/kind.yaml" --wait 120s >"${WORK_DIR}/kind.log" 2>&1; then
      check_fail "create kind cluster" "$(tail -20 "${WORK_DIR}/kind.log")"
      return 1
    fi
    check_pass "created kind cluster ${CLUSTER}"
  fi

  "${KIND}" export kubeconfig --name "${CLUSTER}" --kubeconfig "${KUBECONFIG}" >/dev/null 2>&1

  local server
  server="$(kube version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // "unknown"')"
  log "    server version: ${server}"

  # The operator's slices select nodes by zone, so the node needs one.
  local node
  node="$(kube get nodes -o jsonpath='{.items[0].metadata.name}')"
  kube label node "${node}" "${ZONE_LABEL}=${ZONE}" --overwrite >/dev/null
  check_pass "labelled ${node} with ${ZONE_LABEL}=${ZONE}"

  # Without the capacity gate the API server silently drops capacity requests,
  # which would make every allocation assertion below meaningless.
  local probe
  probe="$(kube apply --dry-run=server -o json -f - 2>/dev/null <<EOF || true
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: capacity-probe
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
EOF
)"
  local kept
  kept="$(jq -r --arg k "${GPU_COUNT_CAPACITY}" \
    '.spec.spec.devices.requests[0].exactly.capacity.requests[$k] // ""' <<<"${probe}" 2>/dev/null || true)"
  check_eq "API server preserves consumable capacity requests" "1" "${kept}"
}

install_chart() {
  step "Chart install"
  kube apply -f "${CHART_MAIN}/crds/" >/dev/null
  check_pass "applied CRDs"

  # The operator runs out of cluster below, so only the chart's cluster-scoped
  # objects matter here; a stub token keeps the Secret template happy.
  if helm_cmd upgrade --install "${RELEASE}" "${CHART_MAIN}" \
    --namespace "${NAMESPACE}" --create-namespace \
    --set thunder.apiToken=local-test \
    --set tests.enabled=false \
    --timeout 120s >"${WORK_DIR}/helm.log" 2>&1; then
    check_pass "installed ${RELEASE}"
  else
    check_fail "installed ${RELEASE}" "$(tail -20 "${WORK_DIR}/helm.log")"
    return 1
  fi

  local expression
  expression="$(kube get deviceclass "${DEVICE_CLASS_NAME}" \
    -o jsonpath='{.spec.selectors[0].cel.expression}' 2>/dev/null || true)"
  check_contains "DeviceClass selects driver ${DRIVER_NAME}" "\"${DRIVER_NAME}\"" "${expression}"

  # The DaemonSet must not schedule anywhere: kind has no Thunder GPU nodes.
  local desired
  desired="$(kube -n "${NAMESPACE}" get daemonset -o jsonpath='{.items[0].status.desiredNumberScheduled}' 2>/dev/null || echo "")"
  check_eq "daemon targets no unlabelled nodes" "0" "${desired}"
}

start_stub() {
  step "Stub Thunder API"
  local inventory
  inventory="$(jq -nc --arg zone "${ZONE}" --arg gpu "${GPU_TYPE}" --arg cap "${GPU_CAPACITY}" '{
    zones:   [{zoneId: "zone-local", displayName: $zone, createdAt: "2026-01-01T00:00:00Z"}],
    hosts:   [{hostId: "host-1", zoneId: "zone-local", displayName: "host-1", hostname: "h1",
               gpuType: $gpu, gpuCount: ($cap | tonumber), status: "active"}],
    clients: []
  }')"
  if [[ "${STUB_PORT}" == "0" ]]; then
    STUB_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
  fi

  python3 "${REPO_ROOT}/hack/lib/thunder-stub.py" "${STUB_PORT}" "${inventory}" \
    >"${WORK_DIR}/stub.log" 2>&1 &
  STUB_PID=$!

  if ! retry 15 1 "stub API" -- curl -fsS -o /dev/null "http://127.0.0.1:${STUB_PORT}/api/v1/zones"; then
    check_fail "stub Thunder API started" "$(cat "${WORK_DIR}/stub.log" 2>/dev/null)"
    return 1
  fi

  # Something else answering on this port would silently feed the operator a
  # different inventory than the one asserted against below.
  if ! kill -0 "${STUB_PID}" 2>/dev/null; then
    check_fail "stub Thunder API is ours" \
      "the stub exited but port ${STUB_PORT} still answers: $(cat "${WORK_DIR}/stub.log" 2>/dev/null)"
    return 1
  fi
  local served
  served="$(curl -fsS "http://127.0.0.1:${STUB_PORT}/api/v1/zones" | jq -r '.zones[0].displayName // ""')"
  if [[ "${served}" != "${ZONE}" ]]; then
    check_fail "stub Thunder API is ours" "port ${STUB_PORT} served zone ${served@Q}, want ${ZONE@Q}"
    return 1
  fi
  check_pass "stub Thunder API on :${STUB_PORT} serving ${GPU_CAPACITY}x ${GPU_TYPE} in zone ${ZONE}"
}

run_operator() {
  step "Operator"
  if ! (cd "${REPO_ROOT}" && go build -o "${WORK_DIR}/thunder-dra-operator" ./cmd/thunder-dra-operator) 2>"${WORK_DIR}/build.log"; then
    check_fail "build operator" "$(cat "${WORK_DIR}/build.log")"
    return 1
  fi
  check_pass "built operator"

  # 1,2,4,8 against a 4-GPU zone deliberately exercises the clamp: the API
  # server rejects the whole slice if a valid value exceeds the capacity.
  VALID_GPU_COUNTS="1,2,4,8" \
  THUNDER_API_URL="http://127.0.0.1:${STUB_PORT}" \
  THUNDER_API_TOKEN=stub \
  RECONCILE_INTERVAL=10s \
    "${WORK_DIR}/thunder-dra-operator" -kubeconfig="${KUBECONFIG}" \
    >"${WORK_DIR}/operator.log" 2>&1 &
  OPERATOR_PID=$!

  if retry 60 2 "resource slices" -- driver_slices_exist; then
    check_pass "operator published ResourceSlices"
  else
    check_fail "operator published ResourceSlices" "$(tail -10 "${WORK_DIR}/operator.log")"
    return 1
  fi

  local slice
  slice="$(slice_for_gpu_type "${GPU_TYPE}")"
  if [[ -z "${slice}" ]]; then
    check_fail "a slice advertises ${GPU_TYPE}"
    return 1
  fi
  check_pass "slice ${slice} advertises ${GPU_TYPE}"

  local spec
  spec="$(kube get resourceslice "${slice}" -o json)"
  check_eq "slice driver" "${DRIVER_NAME}" "$(jq -r '.spec.driver' <<<"${spec}")"
  check_eq "slice capacity" "${GPU_CAPACITY}" \
    "$(jq -r --arg k "${GPU_COUNT_CAPACITY}" '.spec.devices[0].capacity[$k].value' <<<"${spec}")"
  check_eq "slice zone attribute" "${ZONE}" \
    "$(jq -r --arg k "${THUNDER_DOMAIN}/zone" '.spec.devices[0].attributes[$k].string' <<<"${spec}")"
  check_eq "slice allows shared allocation" "true" \
    "$(jq -r '.spec.devices[0].allowMultipleAllocations' <<<"${spec}")"

  # 8 must have been dropped: it is larger than the zone's 4 GPUs.
  local valid
  valid="$(jq -r --arg k "${GPU_COUNT_CAPACITY}" \
    '.spec.devices[0].capacity[$k].requestPolicy.validValues | join(",")' <<<"${spec}")"
  check_eq "request policy is clamped to capacity" "1,2,4" "${valid}"
  check_eq "request policy default is a valid value" "1" \
    "$(jq -r --arg k "${GPU_COUNT_CAPACITY}" '.spec.devices[0].capacity[$k].requestPolicy.default' <<<"${spec}")"

  if ! grep -q ERROR "${WORK_DIR}/operator.log"; then
    check_pass "operator reconciled without errors"
  else
    check_fail "operator reconciled without errors" "$(grep ERROR "${WORK_DIR}/operator.log" | tail -3)"
  fi
}

test_allocation() {
  step "Claim allocation"
  local release="local-pod"
  local selector="app.kubernetes.io/instance=${release}"

  if ! helm_cmd upgrade --install "${release}" "${CHART_TEST_POD}" \
    --namespace default \
    --set "driverName=${DRIVER_NAME}" \
    --set "deviceClassName=${DEVICE_CLASS_NAME}" \
    --set "gpu.type=${GPU_TYPE}" \
    --set-string "gpu.count=2" >"${WORK_DIR}/pod.log" 2>&1; then
    check_fail "install pod test chart" "$(tail -10 "${WORK_DIR}/pod.log")"
    return 1
  fi
  check_pass "installed pod test chart requesting 2x ${GPU_TYPE}"

  local claim=""
  if retry 60 2 "generated claim" -- claim_exists_for_label default "${selector}"; then
    claim="$(claim_by_label default "${selector}")"
    check_pass "ResourceClaimTemplate generated ${claim}"
  else
    check_fail "ResourceClaimTemplate generated a claim"
    return 1
  fi

  if retry 90 2 "claim allocation" -- claim_is_allocated default "${claim}"; then
    check_pass "scheduler allocated ${claim}"
  else
    check_fail "scheduler allocated ${claim}" \
      "$(kube -n default describe resourceclaim "${claim}" | tail -10)"
    return 1
  fi

  local result
  result="$(kube -n default get resourceclaim "${claim}" -o json |
    jq '.status.allocation.devices.results[0]')"
  check_eq "claim allocated to ${DRIVER_NAME}" "${DRIVER_NAME}" "$(jq -r '.driver' <<<"${result}")"
  check_eq "claim consumed 2 GPUs" "2" \
    "$(jq -r --arg k "${GPU_COUNT_CAPACITY}" '.consumedCapacity[$k]' <<<"${result}")"
  check_eq "claim bound to the published pool" "${ZONE}/$(printf '%s' "${GPU_TYPE}" | tr '[:upper:]' '[:lower:]')" \
    "$(jq -r '.pool' <<<"${result}")"

  helm_cmd -n default uninstall "${release}" --wait >/dev/null 2>&1 || true
}

test_over_capacity_is_refused() {
  step "Capacity limits"
  local release="local-pod-toobig"
  local selector="app.kubernetes.io/instance=${release}"

  # 8 is not a valid value for a 4-GPU zone, so this claim must never allocate.
  if ! helm_cmd upgrade --install "${release}" "${CHART_TEST_POD}" \
    --namespace default \
    --set "driverName=${DRIVER_NAME}" \
    --set "deviceClassName=${DEVICE_CLASS_NAME}" \
    --set "gpu.type=${GPU_TYPE}" \
    --set-string "gpu.count=8" >"${WORK_DIR}/toobig.log" 2>&1; then
    check_pass "API server rejected an over-capacity request outright"
    return 0
  fi

  local claim=""
  if retry 30 2 "generated claim" -- claim_exists_for_label default "${selector}"; then
    claim="$(claim_by_label default "${selector}")"
  fi
  if [[ -z "${claim}" ]]; then
    check_pass "no claim was generated for an over-capacity request"
  else
    sleep 20
    if claim_is_allocated default "${claim}"; then
      check_fail "over-capacity claim must not allocate" \
        "${claim} was allocated 8 GPUs from a 4 GPU zone"
    else
      check_pass "over-capacity claim stayed pending"
    fi
  fi
  helm_cmd -n default uninstall "${release}" --wait >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------

step "Thunder device plugin local integration test"
log "    cluster:     ${CLUSTER} (${NODE_IMAGE})"
log "    driver:      ${DRIVER_NAME}"
log "    gpu:         ${GPU_CAPACITY}x ${GPU_TYPE} in zone ${ZONE} (stub inventory)"

create_cluster
install_chart
start_stub
run_operator
test_allocation
test_over_capacity_is_refused

summarize "test-local"
