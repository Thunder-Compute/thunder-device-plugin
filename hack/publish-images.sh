#!/usr/bin/env bash
#
# publish-images.sh - publish both component images and update the chart with
# the tag that was actually published.
#
# The image publication must happen before the values change is committed. The
# resulting Git commit is therefore a complete chart composition: its values
# point at the exact image artifacts that were built and pushed.
#
# Usage: hack/publish-images.sh <image-tag>

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

tag="${1:-}"
[[ "${tag}" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] ||
  die "image tag must be a UTC timestamp (YYYYMMDDTHHMMSSZ), got: ${tag:-<empty>}"

cd "${REPO_ROOT}"

step "Publish images"
make --no-print-directory image-buildx IMAGE_TAG="${tag}"

step "Record image tag in chart values"
hack/update-image-tags.sh "${tag}"

log "published daemon and operator with tag ${tag}"
log "review and commit charts/thunder-device-plugin/values.yaml before pushing"
