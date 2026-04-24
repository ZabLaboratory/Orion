#!/usr/bin/env bash
# Extract the section for a given version from CHANGELOG.md.
#
# Usage: scripts/extract-changelog.sh v0.1.0
#
# Outputs everything between the matching `## [x.y.z]` header (exclusive)
# and the next `## [` header (exclusive). Prints nothing if no such
# section exists — the caller decides how to react.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <tag>" >&2
  exit 2
fi

tag="$1"
# Strip leading `v` if present — CHANGELOG entries are `[0.1.0]`, tags are `v0.1.0`.
version="${tag#v}"

awk -v ver="$version" '
  $0 ~ "^## \\[" ver "\\]" { capture = 1; next }
  capture && /^## \[/      { exit }
  capture                  { print }
' CHANGELOG.md
