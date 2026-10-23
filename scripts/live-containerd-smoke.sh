#!/usr/bin/env bash
# Live containerd smoke: the base row runs on a stock image; each prepared
# pod-crabbox* image runs only when it is already present on the host, and
# otherwise skips VISIBLY by image name. A host without a reachable containerd
# daemon, or without the kernel support the CNI portmap plugin needs, skips
# visibly with the mechanism. Nothing is mocked (R8).
#
# CRABBOX_LIVE_CONTAINERD_STRICT=1 turns every skip into a non-zero exit, so a
# CI row cannot pass on a host that never ran the lease. CRABBOX_LIVE_CONTAINERD_IMAGES=0
# runs only the core base row (no prepared-image rows).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
crabbox="${CRABBOX_BIN:-$root/bin/crabbox}"
if [[ ! -x "$crabbox" ]]; then
  crabbox="$(command -v crabbox || true)"
fi

log() { printf '[live-containerd-smoke] %s\n' "$*" >&2; }

strict="${CRABBOX_LIVE_CONTAINERD_STRICT:-0}"

skip() {
  log "skip: $*"
  if [[ "$strict" == "1" ]]; then
    log "strict: a skip is not a pass (CRABBOX_LIVE_CONTAINERD_STRICT=1)"
    exit 1
  fi
  exit 0
}

# A row-level skip: in strict mode the row fails instead of returning success.
row_skip() {
  log "skip: $*"
  if [[ "$strict" == "1" ]]; then
    log "strict: a skip is not a pass (CRABBOX_LIVE_CONTAINERD_STRICT=1)"
    return 1
  fi
  return 0
}

# Cleanup is armed before the first mutation. `crabbox run` releases its lease
# on success but can leave it behind on failure, so every lease a failed row
# reported is stopped on exit.
created_leases=()
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$crabbox" ]]; then
    local lease
    for lease in ${created_leases[@]+"${created_leases[@]}"}; do
      log "cleanup: stopping orphaned lease $lease"
      "$crabbox" stop --provider containerd "$lease" >/dev/null 2>&1 || true
    done
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ -z "$crabbox" ]]; then
  skip "no crabbox binary (set CRABBOX_BIN)"
fi

if ! command -v nerdctl >/dev/null 2>&1; then
  skip "nerdctl is not on PATH; install nerdctl and a containerd daemon to run this smoke"
fi
# The provider reads CRABBOX_CONTAINERD_ADDRESS/NAMESPACE; the preflight must ask
# the same daemon/namespace or it can skip against the wrong one.
nerdctl_args=()
if [[ -n "${CRABBOX_CONTAINERD_ADDRESS:-}" ]]; then
  nerdctl_args+=(--address "$CRABBOX_CONTAINERD_ADDRESS")
fi
if [[ -n "${CRABBOX_CONTAINERD_NAMESPACE:-}" ]]; then
  nerdctl_args+=(-n "$CRABBOX_CONTAINERD_NAMESPACE")
fi
if ! info="$(nerdctl "${nerdctl_args[@]}" info --format '{{json .}}' 2>/dev/null)"; then
  skip "no reachable containerd daemon for the invoking user; start containerd or point CRABBOX_CONTAINERD_ADDRESS at a running socket"
fi
if ! "$crabbox" providers --json 2>/dev/null | grep -q '"containerd"'; then
  skip "this crabbox binary does not register the containerd provider"
fi

# The provider defaults to the rootful socket and fails closed on a mismatch.
# A rootless daemon is a prepared host; make the smoke exercise that mode.
if [[ "$info" == *'"name=rootless"'* && -z "${CRABBOX_CONTAINERD_ROOTLESS:-}" ]]; then
  log "detected rootless containerd; setting CRABBOX_CONTAINERD_ROOTLESS=true"
  export CRABBOX_CONTAINERD_ROOTLESS=true
fi

# The provider default is crun. A host without crun is still prepared for the
# SSH contract through runc; say which runtime the smoke used.
if [[ -z "${CRABBOX_CONTAINERD_RUNTIME:-}" ]]; then
  if command -v crun >/dev/null 2>&1; then
    export CRABBOX_CONTAINERD_RUNTIME=crun
  elif command -v runc >/dev/null 2>&1; then
    export CRABBOX_CONTAINERD_RUNTIME=runc
    log "crun is not installed; using runc for this smoke"
  else
    skip "neither crun nor runc is available on the containerd host"
  fi
fi

# A rootless bridge lease publishes its SSH port through the CNI portmap
# plugin. Some hosts lack the kernel extension that plugin appends.
portmap_unavailable() {
  local output="$1"
  [[ "$output" == *multiport* || "$output" == *CNI-HOSTPORT-DNAT* ]]
}

# A host that cannot publish ports skips every run row, but the image rows are
# still walked so each absent pod-crabbox* image skips visibly BY NAME instead
# of being hidden behind the first host-level skip.
host_cannot_publish=""

run_row() {
  local label="$1" image="$2"
  shift 2
  local output lease
  local image_args=()
  if [[ -n "$image" ]]; then
    image_args=(--containerd-image "$image")
  fi
  log "$label"
  if [[ -n "$host_cannot_publish" ]]; then
    row_skip "$label — $host_cannot_publish"
    return $?
  fi
  if output="$("$crabbox" run --provider containerd "${image_args[@]}" --ttl 10m --idle-timeout 5m -- "$@" 2>&1)"; then
    if [[ -n "$image" && "$output" != *"image=${image}"* ]]; then
      log "fail: $label ran a different image than ${image}"
      return 1
    fi
    printf '%s\n' "$output"
    return 0
  fi
  # The run failed before it released: record the lease it reported so the EXIT
  # trap stops it. A lease id is only ever appended, never overwritten.
  lease="$(printf '%s\n' "$output" | grep -Eo 'cbx_[a-f0-9]{12}' | head -1 || true)"
  if [[ -n "$lease" ]]; then
    created_leases+=("$lease")
  fi
  if portmap_unavailable "$output"; then
    host_cannot_publish="this host cannot publish the lease SSH port: the CNI portmap plugin needs the xt_multiport kernel extension, which is missing (opencharly/layer-containerd#5); the provider's daemon verification, image pull and argv were correct"
    row_skip "$label — $host_cannot_publish"
    return $?
  fi
  printf '%s\n' "$output" >&2
  return 1
}

run_row "base row: stock image, namespace ${CRABBOX_CONTAINERD_NAMESPACE:-default}" "" \
  sh -c 'echo containerd-live-smoke; uname -s'

image_row() {
  local repo="$1" ref
  ref="$(nerdctl "${nerdctl_args[@]}" images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -m1 -E "^${repo}:" || true)"
  if [[ -z "$ref" ]]; then
    row_skip "image ${repo} is not present on this host (W5 has not published it yet)"
    return $?
  fi
  run_row "image row: ${ref}" "$ref" sh -c 'echo containerd-live-smoke; uname -s'
}

# One row per prepared runner image. The base `pod-crabbox` image is the
# coordinator/SSH contract; the variants add the feature each row names. A CI
# core-only run sets CRABBOX_LIVE_CONTAINERD_IMAGES=0.
if [[ "${CRABBOX_LIVE_CONTAINERD_IMAGES:-1}" == "1" ]]; then
  for repo in \
    ghcr.io/opencharly/pod-crabbox \
    ghcr.io/opencharly/pod-crabbox-systemd \
    ghcr.io/opencharly/pod-crabbox-desktop \
    ghcr.io/opencharly/pod-crabbox-browser \
    ghcr.io/opencharly/pod-crabbox-code \
    ghcr.io/opencharly/pod-crabbox-tailscale \
    ghcr.io/opencharly/pod-crabbox-nested \
    ghcr.io/opencharly/pod-crabbox-k3s \
    ghcr.io/opencharly/pod-crabbox-k3s-krun \
    ghcr.io/opencharly/pod-crabbox-k3s-gpu \
    ghcr.io/opencharly/pod-crabbox-gpu \
    ghcr.io/opencharly/pod-crabbox-criu; do
    image_row "$repo"
  done
fi

log "done"
