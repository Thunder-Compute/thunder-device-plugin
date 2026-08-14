#!/usr/bin/env bash
#
# release-version.sh - print the version this commit would be released as.
#
#   on main:  <chart version>              e.g. 0.2.0
#   anywhere: <chart version>-rc.<n>       e.g. 0.2.0-rc.3
#             where <n> counts commits since main
#
# Usage: hack/release-version.sh [branch]

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
cd "${REPO_ROOT}"

base="$(awk '/^version:/ {print $2; exit}' charts/thunder-device-plugin/Chart.yaml)"
base="${base%%-*}"
[[ "${base}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  die "Chart.yaml version is not semver: ${base}"

branch="${1:-$(git rev-parse --abbrev-ref HEAD)}"
if [[ "${branch}" == "main" ]]; then
  echo "${base}"
  exit 0
fi

main_ref=refs/remotes/origin/main
git rev-parse --verify --quiet "${main_ref}" >/dev/null || main_ref=refs/heads/main
git rev-parse --verify --quiet "${main_ref}" >/dev/null ||
  die "no main branch to count candidates from"

echo "${base}-rc.$(git rev-list --count "${main_ref}..HEAD")"
