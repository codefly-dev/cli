#!/usr/bin/env bash
# Build codefly locally. Install to $GOBIN when set; otherwise leave the
# binary at build/codefly (the case CI hits — no GOBIN, skip the install
# step rather than mv to "/codefly" which blew up with "Permission
# denied"). Callers that want the binary on PATH must set GOBIN.

set -euo pipefail

echo "Installing locally"

mkdir -p build
go build -o build/codefly main.go

if [ -n "${GOBIN:-}" ]; then
    mv build/codefly "$GOBIN/"
    echo "Installed to $GOBIN/codefly"
else
    echo "GOBIN not set — binary at build/codefly (skipping install)"
fi
