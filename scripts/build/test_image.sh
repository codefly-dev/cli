#!/usr/bin/env bash
echo "Build codefly test image"

docker build -t codefly-test -f test/Dockerfile .
