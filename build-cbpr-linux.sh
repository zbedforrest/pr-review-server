#!/bin/bash
# Script to build cbpr for Linux (to be copied into Docker image)
# Run this on your host machine before building the Docker image

set -e

# Get the directory where this script is located
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)

echo "Building cbpr for Linux..."

# Check if cbpr source is available (allow override via environment variable)
CBPR_SRC_PATH=${CBPR_SRC_PATH:-"$SCRIPT_DIR/../cbpr"}
if [ ! -d "$CBPR_SRC_PATH" ]; then
    echo "❌ Error: cbpr source directory not found at '$CBPR_SRC_PATH'"
    echo ""
    echo "Please ensure cbpr source code is available, or set the CBPR_SRC_PATH environment variable."
    echo "Alternatively, manually build a Linux binary and place it at $SCRIPT_DIR/bin/cbpr-linux"
    exit 1
fi

# Create bin directory if it doesn't exist
mkdir -p "$SCRIPT_DIR/bin"

# Build for Linux (using subshell to avoid cd issues)
(
    cd "$CBPR_SRC_PATH"
    GOOS=linux GOARCH=amd64 go build -o "$SCRIPT_DIR/bin/cbpr-linux" .
)

echo "✅ Successfully built cbpr for Linux at $SCRIPT_DIR/bin/cbpr-linux"
echo ""
echo "Now you can build the Docker image:"
echo "  make build"
