#!/usr/bin/env bash
#
# verify-promotion.sh - assert a release and the candidate it was promoted from
# are the same images, byte for byte.
#
# A release re-tags the candidate's manifests rather than rebuilding, so every
# image digest must match. A mismatch means the artifact customers get is not
# the artifact that ran in Thunder's cloud, and the release is not a promotion.
#
# Usage: hack/verify-promotion.sh <candidate> <release>
#        hack/verify-promotion.sh 0.2.0-rc.3 0.2.0

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
cd "${REPO_ROOT}"

candidate="${1:-}"
release="${2:-}"
[[ -n "${candidate}" && -n "${release}" ]] || die "usage: $(basename "$0") <candidate> <release>"

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
  from="$(digest "${REGISTRY}/${image}:${candidate}")"
  to="$(digest "${REGISTRY}/${image}:${release}")"
  if [[ "${from}" == "${to}" ]]; then
    check_pass "${image} ${release} is ${candidate} (${from})"
  else
    check_fail "${image} ${release} is ${candidate}" "candidate ${from} != release ${to}"
  fi
done

summarize "verify-promotion"
