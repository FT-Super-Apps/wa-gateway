#!/usr/bin/env bash
# Bump the project version and cut the CHANGELOG release section.
#
#   scripts/bump-version.sh <patch|minor|major|none|X.Y.Z> [options]
#
# Options:
#   --date YYYY-MM-DD   release date (default: today, UTC)
#   --build N           CI build number to record in the heading
#   --commit SHA        short SHA to mention when the Unreleased section is empty
#   --dry-run           print the new version and planned edits, change nothing
#
# What it does:
#   1. Reads VERSION (single line, semver) and computes the new version.
#   2. Rewrites CHANGELOG.md: the first "## [Unreleased]" heading becomes
#      "## [X.Y.Z] — DATE (build N)", and a fresh empty "## [Unreleased]"
#      section is inserted above it.
#   3. Writes the new version to VERSION and prints it to stdout.
#
# "none" prints the current version and exits without touching any file
# (used by CI to rebuild an existing tag).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION_FILE="$ROOT/VERSION"
CHANGELOG="$ROOT/CHANGELOG.md"
PLACEHOLDER="_Belum ada perubahan yang belum dirilis._"

usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }

[[ $# -ge 1 ]] || usage
bump="$1"; shift

date_str="$(date -u +%Y-%m-%d)"
build_no=""
commit=""
dry_run=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --date)   date_str="$2"; shift 2 ;;
    --build)  build_no="$2"; shift 2 ;;
    --commit) commit="$2"; shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    *) echo "unknown option: $1" >&2; usage ;;
  esac
done

current="$(tr -d '[:space:]' < "$VERSION_FILE")"
if ! [[ "$current" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  echo "VERSION file is not X.Y.Z: '$current'" >&2; exit 1
fi
major="${BASH_REMATCH[1]}"; minor="${BASH_REMATCH[2]}"; patch="${BASH_REMATCH[3]}"

case "$bump" in
  none)  echo "$current"; exit 0 ;;
  major) major=$((major+1)); minor=0; patch=0 ;;
  minor) minor=$((minor+1)); patch=0 ;;
  patch) patch=$((patch+1)) ;;
  *)
    if [[ "$bump" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
      IFS=. read -r major minor patch <<< "$bump"
    else
      echo "invalid bump: '$bump'" >&2; usage
    fi ;;
esac
new="${major}.${minor}.${patch}"

if [[ "$new" == "$current" ]]; then
  echo "new version equals current ($current)" >&2; exit 1
fi

heading="## [${new}] — ${date_str}"
[[ -n "$build_no" ]] && heading="${heading} (build ${build_no})"

empty_note="- Rilis pemeliharaan — tidak ada perubahan yang tercatat di changelog"
[[ -n "$commit" ]] && empty_note="${empty_note} (commit ${commit})"

if ! grep -qE '^## \[Unreleased\]' "$CHANGELOG"; then
  echo "CHANGELOG.md has no '## [Unreleased]' section" >&2; exit 1
fi

new_changelog="$(awk -v heading="$heading" -v placeholder="$PLACEHOLDER" -v empty_note="$empty_note" '
  BEGIN { done = 0; in_release = 0 }
  !done && /^## \[Unreleased\]/ {
    print "## [Unreleased]"
    print ""
    print placeholder
    print ""
    print "---"
    print ""
    print heading
    done = 1; in_release = 1
    next
  }
  in_release && /^## / { in_release = 0 }
  in_release && $0 == placeholder { print empty_note; next }
  { print }
' "$CHANGELOG")"

if [[ $dry_run -eq 1 ]]; then
  echo "would bump $current -> $new" >&2
  echo "would write heading: $heading" >&2
  echo "$new"
  exit 0
fi

printf '%s\n' "$new_changelog" > "$CHANGELOG"
printf '%s\n' "$new" > "$VERSION_FILE"
echo "$new"
