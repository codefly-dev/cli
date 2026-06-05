#!/usr/bin/env bash
# Build codefly locally. Install to $GOBIN when set; otherwise leave the
# binary at build/codefly (the case CI hits — no GOBIN, skip the install
# step rather than mv to "/codefly" which blew up with "Permission
# denied"). Callers that want the binary on PATH must set GOBIN.

set -euo pipefail

# Resolve the repo root from this script's location so the dependency
# preflight works regardless of the caller's CWD. The build steps below
# still run from the repo root (relative `build/` and `main.go`).
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." >/dev/null 2>&1 && pwd)"

# Preflight: validate the build toolchain (Go) before invoking it, so a
# missing/old Go gives a clear message instead of a raw "command not found".
# Skip with CODEFLY_SKIP_DEPCHECK=1 (e.g. CI with a pinned toolchain).
if [[ "${CODEFLY_SKIP_DEPCHECK:-0}" != "1" && -x "${ROOT}/scripts/check-deps.sh" ]]; then
  "${ROOT}/scripts/check-deps.sh" --required-only --quiet || exit 1
fi

echo "Installing locally"

cd "${ROOT}"
mkdir -p build
go build -o build/codefly main.go

if [ -n "${GOBIN:-}" ]; then
    mv build/codefly "$GOBIN/"
    echo "✓ Installed to $GOBIN/codefly"
else
    # No GOBIN → the binary stays at build/codefly and is NOT on PATH, so
    # `codefly ...` (e.g. `codefly run service mind`) will be "command not
    # found". Tell the caller exactly how to get it on PATH.
    echo "GOBIN unset — binary left at ${ROOT}/build/codefly (not on PATH)."
    echo "To install it on your PATH, either:"
    echo "  • set GOBIN and re-run:  GOBIN=\"\$(go env GOPATH)/bin\" bash scripts/build/local.sh"
    echo "    (\$(go env GOPATH)/bin is usually already on PATH)"
    echo "  • or copy it somewhere on PATH:  cp build/codefly \"\$(go env GOPATH)/bin/\""
fi
