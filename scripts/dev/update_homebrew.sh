#!/bin/bash

# Exit if any command fails
set -e

# Check if the version tag is provided
if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <version-tag>"
    exit 1
fi

VERSION_TAG=$1
REPO_NAME="codefly-dev/cli-releases" # Replace with your GitHub repository name
FORMULA_NAME="codefly.rb" # Replace with your formula file name

# Define URLs and binary names for both architectures
declare -A urls
urls["amd64"]="https://github.com/${REPO_NAME}/releases/download/${VERSION_TAG}/codefly-darwin-amd64"
urls["arm64"]="https://github.com/${REPO_NAME}/releases/download/${VERSION_TAG}/codefly-darwin-arm64"

# Loop over architectures and update the formula
for arch in "${!urls[@]}"; do
    binary_name="codefly-darwin-${arch}"
    echo "Downloading ${urls[$arch]}..."
    curl -L -o "${binary_name}" "${urls[$arch]}"
    sha256=$(shasum -a 256 "${binary_name}" | awk '{ print $1 }')

    # Update the formula file with the new sha256 for each architecture
    echo "Updating formula ${FORMULA_NAME} for ${arch}..."
    if [ "$arch" == "amd64" ]; then
        sed -i '' "s|url \".*amd64\"|url \"${urls[$arch]}\"|" "${FORMULA_NAME}"
        sed -i '' "s|sha256 \".*amd64\"|sha256 \"${sha256}\" # amd64|" "${FORMULA_NAME}"
    elif [ "$arch" == "arm64" ]; then
        sed -i '' "s|url \".*arm64\"|url \"${urls[$arch]}\"|" "${FORMULA_NAME}"
        sed -i '' "s|sha256 \".*arm64\"|sha256 \"${sha256}\" # arm64|" "${FORMULA_NAME}"
    fi

    # Clean up downloaded binary
    rm "${binary_name}"
done

echo "Formula ${FORMULA_NAME} updated to version ${VERSION_TAG}"
