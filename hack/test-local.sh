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
# A second model in the same zone is what makes GPU type selection meaningful:
# a request that does not pin a type can be served with a mix of the two.
OTHER_GPU_TYPE="H100"
OTHER_GPU_CAPACITY=2
ZONE="local-zone"
NAMESPACE="thunder-system"
RELEASE="thunder-device-plugin"
GPU_CAPACITY=4
TYPED_EXTENDED_RESOURCE="${THUNDER_DOMAIN}/gpu-a6000"
TYPED_DEVICE_CLASS="thunder-gpu-a6000"
OTHER_EXTENDED_RESOURCE="${THUNDER_DOMAIN}/gpu-h100"
OTHER_DEVICE_CLASS="thunder-gpu-h100"

RECONCILE_SECONDS=10
LOCAL_IMAGE_PREFIX="thunder-device-plugin-local"
LOCAL_IMAGE_TAG="test"
CLUSTER="${CLUSTER:-thunder-local}"
# 1.36 is the first release where DRAExtendedResource is beta and on by default.
# The kind config enables it explicitly so older images work too.
DEFAULT_NODE_IMAGE="kindest/node:v1.36.1"
NODE_IMAGE="${NODE_IMAGE:-${DEFAULT_NODE_IMAGE}}"
# MIN_KIND_VERSION is the kind release DEFAULT_NODE_IMAGE ships with, and moves
# with it. An older kind creates the cluster from a newer node image and only
# fails later, loading images into it, with "unknown containerd config version"
# — which says nothing about the version skew that caused it.
MIN_KIND_VERSION="v0.32.0"
STUB_PORT="${STUB_PORT:-0}"
STUB_HOST=""
STUB_URL=""
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

require_cmd docker kubectl helm go jq curl
if ! command -v "${KIND}" >/dev/null 2>&1; then
  die "kind is required: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
fi

# Only the node image this script pins carries a version requirement with it.
# An explicit --node-image is someone testing an older Kubernetes on the kind
# that ships it, which is exactly what the feature gate in the kind config is
# there for.
if [[ "${NODE_IMAGE}" == "${DEFAULT_NODE_IMAGE}" ]]; then
  # Lowercase because it is incidental to this check, and because CI exports a
  # KIND_VERSION of its own naming the kind it installed.
  kind_version="$("${KIND}" version 2>/dev/null | awk '{print $2}')"
  [[ -n "${kind_version}" ]] || die "could not read the version of ${KIND}"
  if [[ "$(printf '%s\n%s\n' "${MIN_KIND_VERSION}" "${kind_version}" | sort -V | head -1)" != "${MIN_KIND_VERSION}" ]]; then
    die "kind ${MIN_KIND_VERSION} or newer is required for ${NODE_IMAGE}, found ${kind_version}. Upgrade kind, or pass --node-image for the Kubernetes version your kind ships."
  fi
fi

WORK_DIR="$(mktemp -d)"
export KUBECONFIG="${WORK_DIR}/kubeconfig"

cleanup() {
  local status=$?
  set +e

  if ((status != 0)) || ((CHECKS_FAILED > 0)); then
    step "Diagnostics"
    dump_section "operator log" -- operator_logs
    dump_section "resourceslices" -- kube get resourceslices -o wide
    dump_section "resourceclaims" -- kube get resourceclaims -A -o wide
    dump_section "pods" -- kube get pods -A
  fi

  if "${KEEP}"; then
    # Leave the operator and stub running: a frozen cluster cannot show
    # anything reacting, which is the point of keeping it.
    step "Cluster ${CLUSTER} left running (--keep)"
    log ""
    log "  export KUBECONFIG=${WORK_DIR}/kubeconfig"
    log ""
    log "  What is running:"
    log "    kind cluster    ${CLUSTER} (${NODE_IMAGE})"
    log "    operator        in cluster, from the chart (image ${LOCAL_IMAGE_PREFIX}-operator:${LOCAL_IMAGE_TAG})"
    log "    registry stub   pid ${STUB_PID:-none}, ${STUB_URL} (outside the cluster)"
    log "    inventory       ${WORK_DIR}/inventory.json"
    log ""
    log "  Look around:"
    log "    kubectl get deviceclasses -l app.kubernetes.io/name=thunder-dra-driver \\"
    log "      -o custom-columns=CLASS:.metadata.name,RESOURCE:.spec.extendedResourceName"
    log "    kubectl get resourceslices -o wide"
    log "    kubectl get resourceslice \$(kubectl get resourceslices -o name | head -1 | cut -d/ -f2) -o yaml"
    log ""
    log "  Drive it. The stub re-reads its inventory file on every request, so"
    log "  editing the file makes the operator react within ${RECONCILE_SECONDS}s:"
    log "    # double the GPUs by setting a 2x oversubscription target"
    log "    jq '.oversubscription.defaultOversubscriptionTarget = 2' \\"
    log "      ${WORK_DIR}/inventory.json > /tmp/inv && mv /tmp/inv ${WORK_DIR}/inventory.json"
    log "    kubectl get resourceslices -w"
    log ""
    log "    # enroll a new GPU model and watch its class appear"
    log "    jq '.hosts += [{hostId:\"h9\",zoneId:\"zone-local\",displayName:\"h9\",hostname:\"h9\",gpuType:\"H100\",gpuCount:2,status:\"active\"}]' \\"
    log "      ${WORK_DIR}/inventory.json > /tmp/inv && mv /tmp/inv ${WORK_DIR}/inventory.json"
    log "    kubectl get deviceclasses -w"
    log ""
    log "    kubectl -n ${NAMESPACE} logs -l app.kubernetes.io/component=operator -f"
    log ""
    log "  Tear down when done:"
    log "    kill ${STUB_PID:-} 2>/dev/null"
    log "    ${KIND} delete cluster --name ${CLUSTER}"
    log "    rm -rf ${WORK_DIR}"
    return
  fi

  [[ -n "${OPERATOR_PID}" ]] && kill "${OPERATOR_PID}" 2>/dev/null
  [[ -n "${STUB_PID}" ]] && kill "${STUB_PID}" 2>/dev/null
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
  DRAExtendedResource: true
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

  # The driver publishes plain devices and needs only the GA DRA APIs. The
  # extended resource path does need its own gate, so probe for that.
  local probe kept
  probe="$(kube apply --dry-run=server -o json -f - 2>/dev/null <<EOF || true
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: extended-resource-probe
spec:
  extendedResourceName: ${TYPED_EXTENDED_RESOURCE}
  selectors:
    - cel:
        expression: device.driver == "${DRIVER_NAME}"
EOF
)"
  kept="$(jq -r '.spec.extendedResourceName // ""' <<<"${probe}" 2>/dev/null || true)"
  check_eq "API server supports extended resource mapping" "${TYPED_EXTENDED_RESOURCE}" "${kept}"
}

# build_images builds the real container images and loads them into the cluster,
# so the chart runs the same artifacts a release would ship.
build_images() {
  step "Container images"
  local image
  for image in operator daemon; do
    if (cd "${REPO_ROOT}" && docker build -q \
      -f "containers/${image}/Dockerfile" \
      -t "${LOCAL_IMAGE_PREFIX}-${image}:${LOCAL_IMAGE_TAG}" . ) >"${WORK_DIR}/build-${image}.log" 2>&1; then
      check_pass "built the ${image} image"
    else
      check_fail "built the ${image} image" "$(tail -15 "${WORK_DIR}/build-${image}.log")"
      return 1
    fi
    if "${KIND}" load docker-image "${LOCAL_IMAGE_PREFIX}-${image}:${LOCAL_IMAGE_TAG}" \
      --name "${CLUSTER}" >>"${WORK_DIR}/build-${image}.log" 2>&1; then
      check_pass "loaded the ${image} image into ${CLUSTER}"
    else
      check_fail "loaded the ${image} image into ${CLUSTER}" "$(tail -10 "${WORK_DIR}/build-${image}.log")"
      return 1
    fi
  done
}

install_chart() {
  step "Chart install"
  kube apply -f "${CHART_MAIN}/crds/" >/dev/null
  check_pass "applied CRDs"

  # The chart requires the token Secret to exist first.
  kube create namespace "${NAMESPACE}" --dry-run=client -o yaml | kube apply -f - >/dev/null
  kube -n "${NAMESPACE}" create secret generic thunder-api \
    --from-literal=THUNDER_API_TOKEN=local-test \
    --dry-run=client -o yaml | kube apply -f - >/dev/null

  # Everything the operator needs comes from the chart: image, env, RBAC and
  # service account. Nothing here is injected past it.
  if helm_cmd upgrade --install "${RELEASE}" "${CHART_MAIN}" \
    --namespace "${NAMESPACE}" \
    --set "operator.image.repository=${LOCAL_IMAGE_PREFIX}-operator" \
    --set "operator.image.tag=${LOCAL_IMAGE_TAG}" \
    --set "operator.image.pullPolicy=Never" \
    --set "daemon.image.repository=${LOCAL_IMAGE_PREFIX}-daemon" \
    --set "daemon.image.tag=${LOCAL_IMAGE_TAG}" \
    --set "daemon.image.pullPolicy=Never" \
    --set "operator.reconcileInterval=${RECONCILE_SECONDS}s" \
    --set "thunder.apiURL=${STUB_URL}" \
    --timeout 120s >"${WORK_DIR}/helm.log" 2>&1; then
    check_pass "installed ${RELEASE}"
  else
    check_fail "installed ${RELEASE}" "$(tail -20 "${WORK_DIR}/helm.log")"
    return 1
  fi

  # There must be no catch-all class: an unpinned request could mix GPU models.
  check_eq "chart ships no catch-all DeviceClass" "" \
    "$(kube get deviceclass "${DEVICE_CLASS_NAME}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)"

  # The DaemonSet must not schedule anywhere: kind has no Thunder GPU nodes.
  local desired
  desired="$(kube -n "${NAMESPACE}" get daemonset -o jsonpath='{.items[0].status.desiredNumberScheduled}' 2>/dev/null || echo "")"
  check_eq "daemon targets no unlabelled nodes" "0" "${desired}"
}

# write_inventory <gpu-type:count>... rewrites what the stub Thunder API serves.
# OVERSUBSCRIPTION, if set, is served as the zone's default target.
# The stub re-reads the file per request, so the operator sees the change on its
# next reconcile without restarting anything.
write_inventory() {
  local hosts="[]" index=0
  local entry type count
  for entry in "$@"; do
    type="${entry%%:*}"
    count="${entry##*:}"
    index=$((index + 1))
    hosts="$(jq -c --argjson hosts "${hosts}" --arg id "host-${index}" \
      --arg type "${type}" --arg count "${count}" \
      '$hosts + [{hostId: $id, zoneId: "zone-local", displayName: $id, hostname: $id,
                  gpuType: $type, gpuCount: ($count | tonumber), status: "active"}]' <<<'null')"
  done

  jq -nc --arg zone "${ZONE}" --argjson hosts "${hosts}" \
    --argjson target "${OVERSUBSCRIPTION:-1}" '{
    zones: [{zoneId: "zone-local", displayName: $zone, createdAt: "2026-01-01T00:00:00Z"}],
    hosts: $hosts,
    clients: [],
    oversubscription: {oversubscriptionTargets: [], defaultOversubscriptionTarget: $target}
  }' > "${WORK_DIR}/inventory.json"
}

start_stub() {
  step "Stub Thunder Registry"
  # Start with a single GPU model; the second one is enrolled later to show the
  # operator picking it up on its own.
  write_inventory "${GPU_TYPE}:${GPU_CAPACITY}"

  if ! (cd "${REPO_ROOT}" && go build -o "${WORK_DIR}/thunder-registry-stub" \
    hack/thunder-registry-stub.go) 2>"${WORK_DIR}/stub-build.log"; then
    check_fail "built the registry stub" "$(cat "${WORK_DIR}/stub-build.log")"
    return 1
  fi
  check_pass "built the registry stub"

  # Bound to the kind bridge address, not 0.0.0.0, so the cluster can reach it
  # without exposing the stub to the rest of the network. It runs outside the
  # cluster on purpose: it is scaffolding, not a component.
  STUB_HOST="$(kind_bridge_address)"
  if [[ -z "${STUB_HOST}" ]]; then
    check_fail "find the kind bridge address" "docker network inspect kind returned no IPv4 gateway"
    return 1
  fi

  # Port 0 lets the stub pick a free port and announce it, rather than the
  # script guessing one and racing whatever else is on the machine.
  "${WORK_DIR}/thunder-registry-stub" \
    -addr "${STUB_HOST}:0" \
    -inventory "${WORK_DIR}/inventory.json" \
    >"${WORK_DIR}/stub.addr" 2>"${WORK_DIR}/stub.log" &
  STUB_PID=$!

  if ! retry 15 1 "stub address" -- test -s "${WORK_DIR}/stub.addr"; then
    check_fail "stub Thunder Registry started" "$(cat "${WORK_DIR}/stub.log" 2>/dev/null)"
    return 1
  fi
  STUB_URL="http://$(head -1 "${WORK_DIR}/stub.addr")"

  if ! retry 15 1 "stub API" -- curl -fsS -o /dev/null "${STUB_URL}/api/v1/zones"; then
    check_fail "stub Thunder Registry answering" "$(cat "${WORK_DIR}/stub.log" 2>/dev/null)"
    return 1
  fi
  local served
  served="$(curl -fsS "${STUB_URL}/api/v1/zones" | jq -r '.zones[0].displayName // ""')"
  if [[ "${served}" != "${ZONE}" ]]; then
    check_fail "stub Thunder Registry is ours" "${STUB_URL} served zone ${served@Q}, want ${ZONE@Q}"
    return 1
  fi
  check_pass "stub Thunder Registry on ${STUB_URL} serving ${GPU_CAPACITY}x ${GPU_TYPE} in zone ${ZONE}"
}

run_operator() {
  step "Operator"
  # The operator runs in cluster, from the chart, using the chart's image, env,
  # ServiceAccount and RBAC. A missing permission fails here rather than being
  # masked by an admin kubeconfig.
  if retry 180 3 "operator rollout" -- \
    kube -n "${NAMESPACE}" rollout status "deployment/${RELEASE}-operator" --timeout=20s; then
    check_pass "operator became ready from the chart"
  else
    check_fail "operator became ready from the chart" \
      "$(kube -n "${NAMESPACE}" get pods -o wide; kube -n "${NAMESPACE}" describe deployment "${RELEASE}-operator" | tail -20)"
    return 1
  fi

  if retry 90 2 "resource slices" -- driver_slices_exist; then
    check_pass "operator published ResourceSlices"
  else
    check_fail "operator published ResourceSlices" "$(operator_logs | tail -10)"
    return 1
  fi

  local slice
  slice="$(slice_for_gpu_type "${GPU_TYPE}")"
  if [[ -z "${slice}" ]]; then
    check_fail "a slice advertises ${GPU_TYPE}"
    return 1
  fi
  check_pass "slice ${slice} advertises ${GPU_TYPE}"

  step "Per-GPU-type device classes"
  if retry 60 2 "generated DeviceClass" -- resource_exists deviceclass "${TYPED_DEVICE_CLASS}"; then
    check_pass "operator generated ${TYPED_DEVICE_CLASS}"
  else
    check_fail "operator generated ${TYPED_DEVICE_CLASS}" "$(operator_logs | tail -5)"
    return 1
  fi
  check_eq "class exposes a typed extended resource" "${TYPED_EXTENDED_RESOURCE}" \
    "$(kube get deviceclass "${TYPED_DEVICE_CLASS}" -o jsonpath='{.spec.extendedResourceName}')"
  check_contains "class selector pins the GPU model" '"A6000"' \
    "$(kube get deviceclass "${TYPED_DEVICE_CLASS}" -o jsonpath='{.spec.selectors[0].cel.expression}')"
  # Only the enrolled model has a class; the second one appears later, when it
  # is added to inventory.
  check_eq "no class for a GPU type that is not in inventory" "" \
    "$(kube get deviceclass "${OTHER_DEVICE_CLASS}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)"

  step "Operator health"
  local errors
  errors="$(operator_logs | grep ERROR | tail -3 || true)"
  if [[ -z "${errors}" ]]; then
    check_pass "operator reconciled without errors"
  else
    check_fail "operator reconciled without errors" "${errors}"
  fi
}

# Changing Thunder state is the node daemon's job. If the operator ever writes,
# a bug in it could enroll or revoke real hardware, so assert it never does.
test_operator_only_reads() {
  step "Operator is read-only against Thunder"
  local writes
  writes="$(curl -fsS "${STUB_URL}/debug/writes" 2>/dev/null || echo "unavailable")"
  if [[ "${writes}" == "unavailable" ]]; then
    check_fail "the registry stub reported its writes" "could not reach ${STUB_URL}/debug/writes"
    return 1
  fi
  check_eq "the operator made no writes to Thunder" "0" "$(jq -r 'length' <<<"${writes}")"
}

# `helm test` is a real chart feature, so run it the way an operator would,
# once the operator has had a chance to publish.
test_helm_test() {
  step "Chart smoke test"
  if helm_cmd test "${RELEASE}" --namespace "${NAMESPACE}" --timeout 3m >"${WORK_DIR}/helmtest.log" 2>&1; then
    check_pass "the chart's helm test passed in cluster"
  else
    check_fail "the chart's helm test passed in cluster" \
      "$(tail -5 "${WORK_DIR}/helmtest.log"; kube -n "${NAMESPACE}" logs "${RELEASE}-test-inventory" 2>/dev/null | tail -5)"
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
    --set "gpu.count=2" >"${WORK_DIR}/pod.log" 2>&1; then
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

  local results
  results="$(kube -n default get resourceclaim "${claim}" -o json |
    jq '.status.allocation.devices.results')"
  check_eq "claim allocated to ${DRIVER_NAME}" "${DRIVER_NAME}" "$(jq -r '.[0].driver' <<<"${results}")"
  # Two GPUs means two devices now, not one device with a capacity of two.
  check_eq "claim allocated 2 distinct GPUs" "2" "$(jq -r 'length' <<<"${results}")"
  check_eq "allocated GPUs are distinct devices" "2" \
    "$(jq -r '[.[].device] | unique | length' <<<"${results}")"
  check_eq "claim bound to the published pool" "${ZONE}/$(printf '%s' "${GPU_TYPE}" | tr '[:upper:]' '[:lower:]')" \
    "$(jq -r '.[0].pool' <<<"${results}")"

  helm_cmd -n default uninstall "${release}" --wait >/dev/null 2>&1 || true
}

# Extended resource requests are translated by the scheduler into a claim
# against the DeviceClass, so they must still be served from the zone pool.
# Enrolling new hardware must produce a new resource type with no operator
# restart, no chart upgrade and no manual DeviceClass. This is the whole reason
# the classes are generated from inventory rather than templated by the chart.
test_new_gpu_type_appears() {
  step "New GPU type is picked up automatically"

  check_eq "the new model has no class yet" "" \
    "$(kube get deviceclass "${OTHER_DEVICE_CLASS}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)"

  # Enroll a second model, exactly as a new Thunder node would appear.
  write_inventory "${GPU_TYPE}:${GPU_CAPACITY}" "${OTHER_GPU_TYPE}:${OTHER_GPU_CAPACITY}"

  if retry 90 2 "generated DeviceClass" -- resource_exists deviceclass "${OTHER_DEVICE_CLASS}"; then
    check_pass "operator created ${OTHER_DEVICE_CLASS} on its own"
  else
    check_fail "operator created ${OTHER_DEVICE_CLASS} on its own" \
      "$(operator_logs | tail -5)"
    return 1
  fi

  check_eq "the new class exposes its own extended resource" "${OTHER_EXTENDED_RESOURCE}" \
    "$(kube get deviceclass "${OTHER_DEVICE_CLASS}" -o jsonpath='{.spec.extendedResourceName}')"
  check_contains "the new class pins its model" '"H100"' \
    "$(kube get deviceclass "${OTHER_DEVICE_CLASS}" -o jsonpath='{.spec.selectors[0].cel.expression}')"

  local slice
  if retry 60 2 "new pool" -- slice_exists_for_gpu_type "${OTHER_GPU_TYPE}"; then
    slice="$(slice_for_gpu_type "${OTHER_GPU_TYPE}")"
    check_pass "operator published a pool for ${OTHER_GPU_TYPE} (${slice})"
  else
    check_fail "operator published a pool for ${OTHER_GPU_TYPE}"
    return 1
  fi
  check_eq "new pool has one device per GPU" "${OTHER_GPU_CAPACITY}" \
    "$(kube get resourceslice "${slice}" -o jsonpath='{.spec.devices}' | jq 'length')"

  # The new resource must be usable straight away.
  local pod="new-type-probe"
  kube apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: default
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: registry.k8s.io/pause:3.10
      resources:
        limits:
          ${OTHER_EXTENDED_RESOURCE}: "1"
EOF
  if retry 90 2 "claim for the new type" -- claim_with_prefix_is_allocated default "${pod}"; then
    check_pass "a pod can request ${OTHER_EXTENDED_RESOURCE} immediately"
  else
    check_fail "a pod can request ${OTHER_EXTENDED_RESOURCE} immediately" \
      "$(kube -n default describe pod "${pod}" | tail -6)"
  fi
  kube -n default delete pod "${pod}" --ignore-not-found --wait=false >/dev/null 2>&1
}

# Retiring the last node of a model must clean the resource type up again.
test_removed_gpu_type_is_pruned() {
  step "Retired GPU type is cleaned up"
  write_inventory "${GPU_TYPE}:${GPU_CAPACITY}"

  if retry 90 2 "class removal" -- resource_gone deviceclass "${OTHER_DEVICE_CLASS}"; then
    check_pass "operator deleted ${OTHER_DEVICE_CLASS} when its GPUs went away"
  else
    check_fail "operator deleted ${OTHER_DEVICE_CLASS} when its GPUs went away"
  fi
  if retry 60 2 "pool removal" -- slice_gone_for_gpu_type "${OTHER_GPU_TYPE}"; then
    check_pass "operator deleted the ${OTHER_GPU_TYPE} pool"
  else
    check_fail "operator deleted the ${OTHER_GPU_TYPE} pool"
  fi
  # The surviving model is untouched.
  check_eq "the remaining GPU type is unaffected" "${TYPED_DEVICE_CLASS}" \
    "$(kube get deviceclass "${TYPED_DEVICE_CLASS}" -o jsonpath='{.metadata.name}' 2>/dev/null || true)"
}

# The typed extended resource is the point of the per-GPU-type classes: it must
# be served from the zone pool AND never mix models.
test_extended_resource() {
  step "Extended resource"
  local pod="extended-resource-probe"

  kube apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: default
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: registry.k8s.io/pause:3.10
      resources:
        limits:
          ${TYPED_EXTENDED_RESOURCE}: "2"
EOF

  local claim=""
  if retry 90 2 "extended resource claim" -- claim_with_prefix_is_allocated default "${pod}"; then
    claim="$(allocated_claim_with_prefix default "${pod}")"
    check_pass "scheduler generated and allocated ${claim}"
  else
    check_fail "typed extended resource was allocated" \
      "$(kube -n default describe pod "${pod}" | tail -8)"
    kube -n default delete pod "${pod}" --ignore-not-found >/dev/null 2>&1
    return 1
  fi

  local results
  results="$(kube -n default get resourceclaim "${claim}" -o json |
    jq '.status.allocation.devices.results')"
  check_eq "extended resource served by ${DRIVER_NAME}" "${DRIVER_NAME}" \
    "$(jq -r '.[0].driver' <<<"${results}")"
  check_eq "extended resource got 2 GPUs" "2" "$(jq -r 'length' <<<"${results}")"
  # The zone also serves H100s, so this is the assertion that matters: the
  # resource name pinned the model.
  check_eq "every GPU is the requested model" "1" \
    "$(jq -r '[.[].pool] | unique | length' <<<"${results}")"
  check_eq "extended resource drawn from the ${GPU_TYPE} zone pool" \
    "${ZONE}/$(printf '%s' "${GPU_TYPE}" | tr '[:upper:]' '[:lower:]')" \
    "$(jq -r '.[0].pool' <<<"${results}")"

  # A device plugin would advertise the resource on the node; DRA does not.
  local advertised
  advertised="$(kube get nodes -o json |
    jq -r --arg r "${TYPED_EXTENDED_RESOURCE}" '[.items[].status.allocatable[$r]?] | map(select(. != null)) | length')"
  check_eq "resource is scheduler-resolved, not node-advertised" "0" "${advertised}"

  kube -n default delete pod "${pod}" --ignore-not-found --wait=false >/dev/null 2>&1
}

# Asking for more GPUs of one model than that model has, while the zone still
# has other GPUs free, must not silently borrow the other model.
test_typed_request_does_not_borrow_other_models() {
  step "GPU type isolation"
  local pod="type-isolation-probe"

  kube apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: default
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: registry.k8s.io/pause:3.10
      resources:
        limits:
          ${TYPED_EXTENDED_RESOURCE}: "$((GPU_CAPACITY + OTHER_GPU_CAPACITY))"
EOF

  sleep 25
  if claim_with_prefix_is_allocated default "${pod}"; then
    local claim
    claim="$(allocated_claim_with_prefix default "${pod}")"
    check_fail "typed request must not borrow other GPU models" \
      "$(kube -n default get resourceclaim "${claim}" -o json |
         jq -r '[.status.allocation.devices.results[].pool] | unique | join(", ")')"
  else
    check_pass "request for more ${GPU_TYPE}s than exist stayed pending"
  fi
  kube -n default delete pod "${pod}" --ignore-not-found --wait=false >/dev/null 2>&1
}

test_over_capacity_is_refused() {
  step "Capacity limits"
  local release="local-pod-toobig"
  local selector="app.kubernetes.io/instance=${release}"

  # The zone publishes GPU_CAPACITY devices, so a larger claim cannot allocate.
  if ! helm_cmd upgrade --install "${release}" "${CHART_TEST_POD}" \
    --namespace default \
    --set "driverName=${DRIVER_NAME}" \
    --set "deviceClassName=${DEVICE_CLASS_NAME}" \
    --set "gpu.type=${GPU_TYPE}" \
    --set "gpu.count=$((GPU_CAPACITY + 4))" >"${WORK_DIR}/toobig.log" 2>&1; then
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
        "${claim} was allocated more GPUs than the ${GPU_CAPACITY} the zone has"
    else
      check_pass "over-capacity claim stayed pending"
    fi
  fi
  helm_cmd -n default uninstall "${release}" --wait >/dev/null 2>&1 || true
}

# Oversubscription is capacity policy pulled from the Thunder API, not a chart
# setting, so changing it upstream must change the published GPU count with no
# redeploy.
test_oversubscription_from_api() {
  step "Oversubscription from the Thunder API"

  local slice
  slice="$(slice_for_gpu_type "${GPU_TYPE}")"
  check_eq "1x target publishes the physical GPU count" "${GPU_CAPACITY}" \
    "$(kube get resourceslice "${slice}" -o json | jq -r '.spec.devices | length')"

  # 4 physical GPUs at 1.5x becomes 6 published GPUs.
  OVERSUBSCRIPTION=1.5 write_inventory "${GPU_TYPE}:${GPU_CAPACITY}"
  local want=$(( GPU_CAPACITY * 3 / 2 ))
  if retry 90 2 "oversubscribed pool" -- pool_device_count_is "${GPU_TYPE}" "${want}"; then
    check_pass "raising the API target to 1.5x published ${want} GPUs"
  else
    check_fail "raising the API target to 1.5x published ${want} GPUs" \
      "got $(kube get resourceslice "$(slice_for_gpu_type "${GPU_TYPE}")" -o json | jq -r '.spec.devices | length')"
    return 1
  fi

  slice="$(slice_for_gpu_type "${GPU_TYPE}")"
  check_eq "the target is recorded on the slice" "1.5" \
    "$(kube get resourceslice "${slice}" -o json |
       jq -r --arg k "${THUNDER_DOMAIN}/oversubscription" '.metadata.labels[$k] // ""')"
  check_eq "oversubscribed GPUs stay exclusive devices" "null" \
    "$(kube get resourceslice "${slice}" -o json | jq -r '.spec.devices[0].capacity // "null"')"

  # Lowering it again must shrink the pool.
  OVERSUBSCRIPTION=1 write_inventory "${GPU_TYPE}:${GPU_CAPACITY}"
  if retry 90 2 "pool shrink" -- pool_device_count_is "${GPU_TYPE}" "${GPU_CAPACITY}"; then
    check_pass "lowering the target back to 1x shrank the pool"
  else
    check_fail "lowering the target back to 1x shrank the pool"
  fi
}

# ---------------------------------------------------------------------------

step "Thunder device plugin local integration test"
log "    cluster:     ${CLUSTER} (${NODE_IMAGE})"
log "    driver:      ${DRIVER_NAME}"
log "    gpu:         ${GPU_CAPACITY}x ${GPU_TYPE} in zone ${ZONE} (stub inventory)"

create_cluster
build_images
start_stub
install_chart
run_operator
test_operator_only_reads
test_helm_test
test_allocation
test_new_gpu_type_appears
test_extended_resource
test_typed_request_does_not_borrow_other_models
test_over_capacity_is_refused
test_removed_gpu_type_is_pruned
test_oversubscription_from_api

summarize "test-local"
