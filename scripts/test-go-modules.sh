#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
found=0
modules_file="$(mktemp)"
cleanup() {
  rm -f "$modules_file"
}
trap cleanup EXIT

if ! find "$ROOT" \
  \( -path "$ROOT/.git" -o -path "*/node_modules" -o -path "*/dist" -o -path "*/dist-cloudflare" \) -prune \
  -o -type f -name go.mod -print0 >"$modules_file"; then
  printf 'failed to discover go.mod files under %s\n' "$ROOT" >&2
  exit 1
fi

while IFS= read -r -d '' modfile; do
  dir="$(dirname "$modfile")"
  rel="${dir#"$ROOT"/}"
  if [[ "$dir" == "$ROOT" ]]; then
    rel="."
  fi
  found=1
  # The root module also contains internal/cli, the slowest package in the tree.
  # It is covered AUTHORITATIVELY by the four CLI race shards
  # (scripts/test-go-shard.py selects every top-level Test/Example/Fuzz) and by
  # the CLI coverage shards. Running the whole package here as well duplicates
  # that coverage and shares ONE 20-minute package budget: on a 2-vCPU runner it
  # consumes ~19 of those 20 minutes, so the 64-second coordinator-stop contract
  # inherited a 48-second deadline and panicked (CI run 39961798509). This job
  # tests the rest of the module; the shards own internal/cli.
  packages=(./...)
  if [[ "$rel" == "." ]]; then
    mapfile -t packages < <(cd "$dir" && go list ./... | grep -v '/internal/cli$' || true)
    if [[ "${#packages[@]}" -eq 0 ]]; then
      printf 'the root module resolved no packages after excluding internal/cli\n' >&2
      exit 1
    fi
  fi
  printf '+ (cd %q && go test -timeout=20m %s)\n' "$rel" "${packages[*]}"
  (cd "$dir" && go test -timeout=20m "${packages[@]}")
done <"$modules_file"

if [[ "$found" -eq 0 ]]; then
  printf 'no go.mod files found under %s\n' "$ROOT" >&2
  exit 1
fi
