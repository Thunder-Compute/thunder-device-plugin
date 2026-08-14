#!/usr/bin/env bash
#
# verify-promotion.sh - assert a release and the source image it was promoted from
# are the same images, byte for byte.
#
# A release re-tags the source commit's manifests rather than rebuilding, so every
# image digest must match. A mismatch means the artifact customers get is not
# the artifact that ran in Thunder's cloud, and the release is not a promotion.
#
# Usage: hack/verify-promotion.sh <source-tag> <release>
#        hack/verify-promotion.sh 4b689906dd8b9c59cc581145e2e9876f28405f26 0.2.0

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
cd "${REPO_ROOT}"

source_tag="${1:-}"
release="${2:-}"
[[ -n "${source_tag}" && -n "${release}" ]] || die "usage: $(basename "$0") <source-tag> <release>"

REGISTRY="${REGISTRY:-ghcr.io/thunder-compute/thunder-device-plugin}"

# digest <image:tag> prints the manifest digest without pulling the image.
digest() {
  if command -v crane >/dev/null 2>&1; then
    crane digest "$1"
  else
    docker buildx imagetools inspect "$1" --format '{{.Manifest.Digest}}'
  fi
}

step "Promotion"
for image in daemon operator; do
  from="$(digest "${REGISTRY}/${image}:${source_tag}")"
  to="$(digest "${REGISTRY}/${image}:${release}")"
  if [[ "${from}" == "${to}" ]]; then
    check_pass "${image} ${release} is ${source_tag} (${from})"
  else
    check_fail "${image} ${release} is ${source_tag}" "source ${from} != release ${to}"
  fi
done

summarize "verify-promotion"
