#!/usr/bin/env bash
# Shared helpers for the scripts in hack/. Source, do not execute.

if [[ -n "${_THUNDER_COMMON_SH:-}" ]]; then
  return 0
fi
_THUNDER_COMMON_SH=1

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export REPO_ROOT

KUBECTL="${KUBECTL:-kubectl}"
HELM="${HELM:-helm}"

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  _C_RED=$'\033[0;31m'; _C_GREEN=$'\033[0;32m'; _C_YELLOW=$'\033[0;33m'
  _C_BOLD=$'\033[1m'; _C_OFF=$'\033[0m'
else
  _C_RED=''; _C_GREEN=''; _C_YELLOW=''; _C_BOLD=''; _C_OFF=''
fi

log()  { printf '%s\n' "$*" >&2; }
pass() { printf '%s ok %s %s\n' "${_C_GREEN}" "${_C_OFF}" "$*" >&2; }
warn() { printf '%swarn%s %s\n' "${_C_YELLOW}" "${_C_OFF}" "$*" >&2; }
fail() { printf '%sFAIL%s %s\n' "${_C_RED}" "${_C_OFF}" "$*" >&2; }
die()  { fail "$*"; exit 1; }

step() {
  printf '\n%s%s%s\n' "${_C_BOLD}" "$*" "${_C_OFF}" >&2
}

# usage <script> prints the script's leading comment block as help text.
usage() {
  awk 'NR == 1 && /^#!/ { next }
       /^#/ { sub(/^# ?/, ""); print; next }
       { exit }' "$1"
}

# require_cmd <command>... exits when any command is missing from PATH.
require_cmd() {
  local missing=()
  local cmd
  for cmd in "$@"; do
    command -v "${cmd}" >/dev/null 2>&1 || missing+=("${cmd}")
  done
  if ((${#missing[@]})); then
    die "missing required commands: ${missing[*]}"
  fi
}

# require_env <VAR>... exits when any variable is unset or empty.
require_env() {
  local missing=()
  local name
  for name in "$@"; do
    [[ -n "${!name:-}" ]] || missing+=("${name}")
  done
  if ((${#missing[@]})); then
    die "missing required environment variables: ${missing[*]}"
  fi
}

# Test bookkeeping. check/check_fail record a result and never abort, so one
# run reports every failure instead of only the first.
CHECKS_RUN=0
CHECKS_FAILED=0
FAILED_CHECKS=()

check_pass() {
  CHECKS_RUN=$((CHECKS_RUN + 1))
  pass "$1"
}

check_fail() {
  CHECKS_RUN=$((CHECKS_RUN + 1))
  CHECKS_FAILED=$((CHECKS_FAILED + 1))
  FAILED_CHECKS+=("$1")
  fail "$1"
  if [[ -n "${2:-}" ]]; then
    printf '     %s\n' "$2" >&2
  fi
}

# check <description> -- <command>... runs the command and records the result.
check() {
  local description="$1"
  shift
  [[ "${1:-}" == "--" ]] && shift
  local output
  if output="$("$@" 2>&1)"; then
    check_pass "${description}"
    return 0
  fi
  check_fail "${description}" "${output}"
  return 1
}

# check_eq <description> <want> <got>
check_eq() {
  local description="$1" want="$2" got="$3"
  if [[ "${want}" == "${got}" ]]; then
    check_pass "${description}"
    return 0
  fi
  check_fail "${description}" "want ${want@Q}, got ${got@Q}"
  return 1
}

# check_contains <description> <needle> <haystack>
check_contains() {
  local description="$1" needle="$2" haystack="$3"
  if [[ "${haystack}" == *"${needle}"* ]]; then
    check_pass "${description}"
    return 0
  fi
  check_fail "${description}" "${needle@Q} not found in output"
  return 1
}

# check_not_contains <description> <needle> <haystack>
check_not_contains() {
  local description="$1" needle="$2" haystack="$3"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    check_pass "${description}"
    return 0
  fi
  check_fail "${description}" "${needle@Q} unexpectedly present"
  return 1
}

# summarize <suite name> prints the tally and returns non-zero on any failure.
summarize() {
  local suite="$1"
  printf '\n' >&2
  if ((CHECKS_FAILED == 0)); then
    printf '%s%s: %d/%d checks passed%s\n' "${_C_GREEN}" "${suite}" "${CHECKS_RUN}" "${CHECKS_RUN}" "${_C_OFF}" >&2
    return 0
  fi
  printf '%s%s: %d/%d checks failed%s\n' "${_C_RED}" "${suite}" "${CHECKS_FAILED}" "${CHECKS_RUN}" "${_C_OFF}" >&2
  local failed
  for failed in "${FAILED_CHECKS[@]}"; do
    printf '  - %s\n' "${failed}" >&2
  done
  return 1
}

# retry <timeout-seconds> <interval-seconds> <description> -- <command>...
# Runs the command until it succeeds or the timeout elapses.
retry() {
  local timeout="$1" interval="$2" description="$3"
  shift 3
  [[ "${1:-}" == "--" ]] && shift

  local deadline=$((SECONDS + timeout))
  local output=""
  local attempts=0
  while ((SECONDS < deadline)); do
    attempts=$((attempts + 1))
    if output="$("$@" 2>&1)"; then
      [[ ${attempts} -gt 1 ]] && log "    ${description}: ready after ${attempts} attempts"
      return 0
    fi
    sleep "${interval}"
  done
  log "    ${description}: timed out after ${timeout}s (${attempts} attempts)"
  [[ -n "${output}" ]] && log "    last output: ${output}"
  return 1
}

# kube <args>... runs kubectl with the configured context.
kube() {
  "${KUBECTL}" ${KUBE_CONTEXT:+--context "${KUBE_CONTEXT}"} "$@"
}

# helm_cmd <args>... runs helm with the configured context.
helm_cmd() {
  "${HELM}" ${KUBE_CONTEXT:+--kube-context "${KUBE_CONTEXT}"} "$@"
}

# resource_exists <args>... is true when kubectl can get the resource.
resource_exists() {
  kube get "$@" >/dev/null 2>&1
}

# api_resource_exists <resource> is true when the cluster serves the resource.
api_resource_exists() {
  kube api-resources --no-headers 2>/dev/null | awk '{print $1}' | grep -qx "$1"
}

# dump_section <title> -- <command>... prints a labelled block of diagnostics.
dump_section() {
  local title="$1"
  shift
  [[ "${1:-}" == "--" ]] && shift
  log "--- ${title} ---"
  "$@" 2>&1 | sed 's/^/    /' >&2 || true
}

# dump_diagnostics <namespace> prints the state that explains a failed run.
dump_diagnostics() {
  local namespace="$1"
  step "Diagnostics"
  dump_section "pods in ${namespace}" -- kube -n "${namespace}" get pods -o wide
  dump_section "resourceclaims" -- kube get resourceclaims -A
  dump_section "resourceslices" -- kube get resourceslices
  dump_section "thunder clients" -- kube get clients.thundercompute.com -A
  dump_section "daemon logs" -- kube -n "${namespace}" logs -l app.kubernetes.io/component=daemon --tail=80
  dump_section "operator logs" -- kube -n "${namespace}" logs -l app.kubernetes.io/component=operator --tail=80
  log "--- recent events ---"
  kube get events -A --sort-by=.lastTimestamp 2>&1 | tail -30 | sed 's/^/    /' >&2 || true
}
