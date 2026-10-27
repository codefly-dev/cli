#!/usr/bin/env bash
# Print relevant=true unless every path changed between <base> and HEAD is
# documentation: docs/, or Markdown outside a testdata directory.
#
# Rename detection is off. With it, a moved file shows only its new path, so
# moving a Go source or a fixture into docs/ (or renaming it to .md) would
# read as a documentation-only change and skip the qualification it removed
# code from.
set -euo pipefail

base=${1:?usage: generate-qualification-scope.sh <base>}
changed=$(git diff --no-renames --name-only "$base" HEAD)
relevant=false
while IFS= read -r path; do
  case "$path" in
    '') ;;
    # Fixtures can be Markdown; only prose is exempt.
    testdata/*|*/testdata/*) relevant=true ;;
    docs/*|*.md) ;;
    *) relevant=true ;;
  esac
done <<<"$changed"
echo "relevant=$relevant"
