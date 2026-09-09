#!/usr/bin/env bash
# Fail unless every row a job claimed in CODEFLY_CONFORMANCE_REQUIRED left a
# receipt in CODEFLY_CONFORMANCE_RECEIPTS.
#
# `go test -run <filter>` exits 0 when the filter matches nothing, so a renamed
# or mis-filtered conformance test would otherwise report a green compatibility
# claim for a row that never ran.
set -euo pipefail

required="${CODEFLY_CONFORMANCE_REQUIRED:-}"
receipts="${CODEFLY_CONFORMANCE_RECEIPTS:-}"

if [[ -z "${required}" ]]; then
  echo "no conformance rows were claimed; nothing to verify"
  exit 0
fi
if [[ -z "${receipts}" ]]; then
  echo "CODEFLY_CONFORMANCE_REQUIRED is set but CODEFLY_CONFORMANCE_RECEIPTS is not" >&2
  exit 1
fi

missing=0
IFS=',' read -ra rows <<<"${required}"
for row in "${rows[@]}"; do
  row="${row// /}"
  [[ -z "${row}" ]] && continue
  shopt -s nullglob
  found=("${receipts}/${row}."*.json)
  shopt -u nullglob
  if (( ${#found[@]} == 0 )); then
    echo "no conformance receipt for required row ${row}: it never ran" >&2
    missing=1
    continue
  fi
  echo "row ${row}: ${#found[@]} receipt(s)"
done

exit "${missing}"
