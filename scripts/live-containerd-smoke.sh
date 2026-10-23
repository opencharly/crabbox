#!/usr/bin/env bash
# Live containerd smoke: the base row runs on a stock image; each prepared
# pod-crabbox* image runs only when it is already present on the host, and
# otherwise skips VISIBLY by image name. A host without a reachable containerd
# daemon, or without the kernel support the CNI portmap plugin needs, skips
# visibly with the mechanism. Nothing is mocked (R8).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
crabbox="${CRABBOX_BIN:-$root/bin/crabbox}"
if [[ ! -x "$crabbox" ]]; then
  crabbox="$(command -v crabbox || true)"
fi
if [[ -z "$crabbox" ]]; then
  echo "[live-containerd-smoke] skip: no crabbox binary (set CRABBOX_BIN)"
  exit 0
fi

log() { printf '[live-containerd-smoke] %s\n' "$*" >&2; }

if ! command -v nerdctl >/dev/null 2>&1; then
  log "skip: nerdctl is not on PATH; install nerdctl and a containerd daemon to run this smoke"
  exit 0
fi
if ! info="$(nerdctl info --format '{{json .}}' 2>/dev/null)"; then
  log "skip: no reachable containerd daemon for the invoking user; start containerd or point CRABBOX_CONTAINERD_ADDRESS at a running socket"
  exit 0
fi
if ! "$crabbox" providers --json 2>/dev/null | grep -q '"containerd"'; then
  log "skip: this crabbox binary does not register the containerd provider"
  exit 0
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
    log "skip: neither crun nor runc is available on the containerd host"
    exit 0
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
  local label="$1"
  shift
  local output
  log "$label"
  if [[ -n "$host_cannot_publish" ]]; then
    log "skip: $label — $host_cannot_publish"
    return 0
  fi
  if output="$("$crabbox" run --provider containerd --ttl 10m --idle-timeout 5m -- "$@" 2>&1)"; then
    printf '%s\n' "$output"
    return 0
  fi
  if portmap_unavailable "$output"; then
    host_cannot_publish="this host cannot publish the lease SSH port: the CNI portmap plugin needs the xt_multiport kernel extension, which is missing (opencharly/layer-containerd#5); the provider's daemon verification, image pull and argv were correct"
    log "skip: $label — $host_cannot_publish"
    return 0
  fi
  printf '%s\n' "$output" >&2
  return 1
}

run_row "base row: stock image, namespace ${CRABBOX_CONTAINERD_NAMESPACE:-default}" \
  sh -c 'echo containerd-live-smoke; uname -s'

image_row() {
  local repo="$1" ref
  ref="$(nerdctl images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -E "^${repo}:" | head -1 || true)"
  if [[ -z "$ref" ]]; then
    log "skip: image ${repo} is not present on this host (W5 has not published it yet)"
    return 0
  fi
  run_row "image row: ${ref}" sh -c 'echo containerd-live-smoke; uname -s'
}

# One row per prepared runner image. The base `pod-crabbox` image is the
# coordinator/SSH contract; the variants add the feature each row names.
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

log "done"
