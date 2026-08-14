#!/usr/bin/env bash
#
# update-image-tags.sh - record one CI-published tag for both chart components.
#
# Usage: hack/update-image-tags.sh <image-tag>

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

tag="${1:-}"
[[ "${tag}" =~ ^[a-z0-9_][a-z0-9_.-]{0,127}$ ]] ||
  die "invalid image tag: ${tag:-<empty>}"

values="${REPO_ROOT}/charts/thunder-device-plugin/values.yaml"
temporary="$(mktemp "${values}.tmp.XXXXXX")"
trap 'rm -f "${temporary}"' EXIT

awk -v tag="${tag}" '
  function component_start(name) {
    return $0 == name ":"
  }

  /^[^[:space:]]/ {
    component = ""
    in_image = 0
  }

  component_start("operator") || component_start("daemon") {
    component = $1
    sub(/:$/, "", component)
    in_image = 0
  }

  component != "" && /^  image:/ {
    in_image = 1
  }

  component != "" && in_image && /^  [^[:space:]]/ && $0 !~ /^  image:/ {
    in_image = 0
  }

  component != "" && in_image && /^    tag:/ {
    print "    tag: \"" tag "\""
    updated[component] = 1
    next
  }

  { print }

  END {
    if (!updated["operator"] || !updated["daemon"]) {
      exit 1
    }
  }
' "${values}" >"${temporary}" || die "could not update both image tags in ${values}"

mv "${temporary}" "${values}"
trap - EXIT
printf 'updated operator.image.tag and daemon.image.tag to %s\n' "${tag}"
