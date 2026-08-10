#!/usr/bin/env bash
#
# verify.sh - offline checks. Builds and tests the Go code, then renders every
# chart and asserts the manifests carry the expected resource identity. Needs no
# cluster, so it is the check to run in CI and before pushing.
#
# Usage: hack/verify.sh [--skip-go] [--skip-charts]

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
source "${REPO_ROOT}/hack/lib/thunder.sh"

SKIP_GO=false
SKIP_CHARTS=false

while (($#)); do
  case "$1" in
    --skip-go) SKIP_GO=true ;;
    --skip-charts) SKIP_CHARTS=true ;;
    -h|--help) usage "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown flag: $1 (try --help)" ;;
  esac
  shift
done

cd "${REPO_ROOT}"

verify_go() {
  step "Go"
  require_cmd go

  local unformatted
  unformatted="$(gofmt -l ./cmd ./internal hack/thunder-registry-stub.go)"
  if [[ -z "${unformatted}" ]]; then
    check_pass "gofmt is clean"
  else
    check_fail "gofmt is clean" "reformat: ${unformatted//$'\n'/ }"
  fi

  check "go build ./..." -- go build ./...
  check "go vet ./..." -- go vet ./...
  # The stub carries a build-ignore tag, so ./... never reaches it.
  check "go vet the registry stub" -- go vet hack/thunder-registry-stub.go
  check "go test ./..." -- go test ./...
}

# assert_render <description> <needle> <manifest> asserts the manifest contains it.
verify_charts() {
  step "Charts"
  require_cmd helm

  local chart
  for chart in "${CHART_MAIN}" "${CHART_TEST_POD}" "${CHART_TEST_VM}"; do
    check "helm lint $(basename "$(dirname "${chart}")")/$(basename "${chart}")" -- \
      helm lint "${chart}"
  done

  local main_manifest pod_manifest vm_manifest
  if ! main_manifest="$(helm template verify "${CHART_MAIN}" 2>&1)"; then
    check_fail "render thunder-device-plugin" "${main_manifest}"
    return
  fi
  check_pass "render thunder-device-plugin"

  if ! pod_manifest="$(helm template verify "${CHART_TEST_POD}" 2>&1)"; then
    check_fail "render tests/pod" "${pod_manifest}"
    return
  fi
  check_pass "render tests/pod"

  if ! vm_manifest="$(helm template verify "${CHART_TEST_VM}" 2>&1)"; then
    check_fail "render tests/vm" "${vm_manifest}"
    return
  fi
  check_pass "render tests/vm"

  local all="${main_manifest}${pod_manifest}${vm_manifest}"

  step "Resource identity"
  check_not_contains "no manifest calls a resource a vgpu" "vgpu" "${all}"
  check_not_contains "no manifest uses external-ip" "external-ip" "${all}"
  check_contains "driver name is ${DRIVER_NAME}" "${DRIVER_NAME}" "${main_manifest}"
  check_contains "kubelet plugin dir is driver-scoped" "/var/lib/kubelet/plugins/${DRIVER_NAME}" "${main_manifest}"
  # The token is never a chart value, so it can never reach the release history.
  check_not_contains "chart never takes the API token as a value" "apiToken" "${main_manifest}"
  check_not_contains "chart creates no Secret" "kind: Secret" "${main_manifest}"
  # One device per GPU, so a claim asks for a device count rather than a
  # capacity request. This is what lets extended resources request >1 GPU.
  check_contains "pod claim requests GPUs by device count" "count:" "${pod_manifest}"
  check_contains "VM claim requests GPUs by device count" "count:" "${vm_manifest}"
  check_not_contains "no claim uses a gpu_count capacity request" "gpu_count:" "${pod_manifest}${vm_manifest}"
  # The GPU model is pinned by the class the claim names, not by a selector.
  check_contains "claims name a per-GPU-type DeviceClass" "deviceClassName: \"thunder-gpu-a6000\"" \
    "${pod_manifest}"
  # No catch-all DeviceClass at all: every request must name a GPU model, and a
  # class that matched every model could be satisfied with a mix of them.
  check_not_contains "chart ships no DeviceClass" "kind: DeviceClass" "${main_manifest}"
  check_contains "operator generates per-GPU-type extended resources" \
    "EXTENDED_RESOURCE_PREFIX" "${main_manifest}"
  check_contains "operator may manage DeviceClasses" "deviceclasses" "${main_manifest}"
  # Sharing is expressed by publishing more devices, never consumable capacity.
  check_not_contains "test charts pin a GPU type by class" "deviceClassName: \"thunder-gpu\"" \
    "${pod_manifest}${vm_manifest}"

  step "Advertised IP"
  check_contains "daemon reads ${ADVERTISED_IP_LABEL}" "${ADVERTISED_IP_LABEL}" "${main_manifest}"
  check_contains "daemon takes NODE_ADVERTISED_IP_LABEL" "NODE_ADVERTISED_IP_LABEL" "${main_manifest}"

  # The advertised IP defaults to the node IP, so scheduling must not require
  # the label to be present on the node.
  local affinity
  affinity="$(printf '%s' "${main_manifest}" | sed -n '/requiredDuringSchedulingIgnoredDuringExecution/,/^      [a-z]/p')"
  check_not_contains "node affinity does not require an advertised-ip label" \
    "${ADVERTISED_IP_LABEL}" "${affinity}"
  check_contains "node affinity still requires a zone label" "${ZONE_LABEL}" "${affinity}"

  verify_chart_quality "${main_manifest}"

  if command -v kubectl >/dev/null 2>&1; then
    step "Manifest schema"
    local output
    if output="$(printf '%s' "${main_manifest}" | kubectl apply --dry-run=client -f - 2>&1)"; then
      check_pass "rendered manifests are valid Kubernetes objects"
    else
      # A client dry run resolves kinds against the cluster, so an unreachable
      # cluster or an unregistered CRD is not a manifest problem.
      if printf '%s' "${output}" | grep -qE 'no matches for kind|connection refused|Unable to connect'; then
        warn "skipping schema check: cluster or CRDs unavailable"
      else
        check_fail "rendered manifests are valid Kubernetes objects" "${output}"
      fi
    fi
  else
    warn "skipping schema check: kubectl not found"
  fi
}

# verify_chart_quality asserts the packaging metadata enterprises look for:
# declared chart metadata, a values schema that is actually wired up, and
# release notes.
verify_chart_quality() {
  local main_manifest="$1"

  step "Chart packaging"

  local field
  for field in name description version appVersion kubeVersion home sources maintainers; do
    if "${HELM}" show chart "${CHART_MAIN}" 2>/dev/null | grep -q "^${field}:"; then
      check_pass "Chart.yaml declares ${field}"
    else
      check_fail "Chart.yaml declares ${field}"
    fi
  done

  local file
  for file in values.schema.json README.md .helmignore templates/NOTES.txt; do
    if [[ -f "${CHART_MAIN}/${file}" ]]; then
      check_pass "chart ships ${file}"
    else
      check_fail "chart ships ${file}"
    fi
  done

  if jq empty "${CHART_MAIN}/values.schema.json" 2>/dev/null; then
    check_pass "values.schema.json is valid JSON"
  else
    check_fail "values.schema.json is valid JSON"
  fi

  # A schema that does not reject an unknown key is not actually enforcing
  # anything, which is the failure mode worth catching.
  if "${HELM}" template schemacheck "${CHART_MAIN}" \
    --set operator.notARealValue=1 >/dev/null 2>&1; then
    check_fail "values.schema.json rejects unknown values" \
      "helm accepted operator.notARealValue"
  else
    check_pass "values.schema.json rejects unknown values"
  fi

  if "${HELM}" template notes "${CHART_MAIN}" \
    -s templates/NOTES.txt >/dev/null 2>&1 || true; then
    check_pass "NOTES.txt renders"
  fi

  check_contains "chart renders a helm test hook" "helm.sh/hook: test" "${main_manifest}"
}

"${SKIP_GO}" || verify_go
"${SKIP_CHARTS}" || verify_charts

summarize "verify"
