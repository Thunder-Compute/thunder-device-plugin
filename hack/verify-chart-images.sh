#!/usr/bin/env bash
#
# verify-chart-images.sh - assert that the component images selected by the
# chart exist in the registry and print their immutable manifest digests.

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

cd "${REPO_ROOT}"
values="charts/thunder-device-plugin/values.yaml"

image_field() {
  local component="$1"
  local field="$2"
  awk -v component="${component}" -v field="${field}" '
    $0 == component ":" { in_component = 1; next }
    in_component && /^[^ ]/ { exit }
    in_component && $1 == field ":" {
      value = $2
      gsub(/^['\''"]|['\''"]$/, "", value)
      print value
      exit
    }
  ' "${values}"
}

digest() {
  if command -v crane >/dev/null 2>&1; then
    crane digest "$1"
  elif command -v docker >/dev/null 2>&1; then
    docker buildx imagetools inspect "$1" --format '{{.Manifest.Digest}}'
  else
    die "crane or docker with buildx is required"
  fi
}

step "Chart images"
common_tag=""
for component in daemon operator; do
  repository="$(image_field "${component}" repository)"
  tag="$(image_field "${component}" tag)"
  [[ -n "${repository}" && -n "${tag}" ]] ||
    die "${component}.image.repository and tag must be set in ${values}"
  [[ "${tag}" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] ||
    die "${component}.image.tag is not a UTC build timestamp: ${tag}"
  if [[ -n "${common_tag}" && "${tag}" != "${common_tag}" ]]; then
    die "daemon and operator do not use the same image tag"
  fi
  common_tag="${tag}"

  reference="${repository}:${tag}"
  manifest="$(digest "${reference}")"
  [[ "${manifest}" =~ ^sha256:[a-f0-9]{64}$ ]] ||
    die "could not resolve a manifest digest for ${reference}: ${manifest}"
  pass "${reference} exists (${manifest})"
done
