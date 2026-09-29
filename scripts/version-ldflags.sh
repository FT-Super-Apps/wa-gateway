#!/usr/bin/env bash
# Prints the -ldflags value that stamps build metadata into the Go binary.
#
#   go build -ldflags "$(scripts/version-ldflags.sh)" .
#
# Env overrides (used by CI): VERSION, COMMIT, BUILD_TIME, BUILD_NUMBER.
# Without overrides: VERSION file + "-dev" suffix, git short SHA (+ "-dirty"),
# current UTC time, build number 0.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

ver="${VERSION:-}"
if [[ -z "$ver" ]]; then
  ver="$(tr -d '[:space:]' < "$ROOT/VERSION")-dev"
fi

commit="${COMMIT:-}"
if [[ -z "$commit" ]]; then
  commit="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  if [[ "$commit" != "unknown" ]] && ! git -C "$ROOT" diff --quiet HEAD -- 2>/dev/null; then
    commit="${commit}-dirty"
  fi
fi

build_time="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
build_number="${BUILD_NUMBER:-0}"

pkg="wa-gateway/pkg/version"
printf -- "-X %s.Version=%s -X %s.Commit=%s -X %s.BuildTime=%s -X %s.BuildNumber=%s\n" \
  "$pkg" "$ver" "$pkg" "$commit" "$pkg" "$build_time" "$pkg" "$build_number"
