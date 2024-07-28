#!/usr/bin/env bash
echo "Build codefly CLI image"

YAML_FILE="pkg/cli/info.yaml"

if [ ! -f "$YAML_FILE" ]; then
    echo "Error: YAML file $YAML_FILE does not exist."
    exit 1
fi

CURRENT_VERSION=$(yq -r '.version' "$YAML_FILE")

docker build -t codeflydev/codefly:"${CURRENT_VERSION}" -f test/Dockerfile .
