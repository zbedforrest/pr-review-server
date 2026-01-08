#!/bin/bash
# Script to build cbpr for Linux (to be copied into Docker image)
# Run this on your host machine before building the Docker image

set -e

echo "Building cbpr for Linux..."

# Check if cbpr source is available
if [ ! -d "../cbpr" ]; then
    echo "❌ Error: cbpr source directory not found at ../cbpr"
    echo ""
    echo "Please ensure cbpr source code is available, or manually build"
    echo "a Linux binary and place it at ./bin/cbpr-linux"
    exit 1
fi

# Create bin directory if it doesn't exist
mkdir -p ./bin

# Build for Linux
cd ../cbpr
GOOS=linux GOARCH=amd64 go build -o ../pr-review-server/bin/cbpr-linux .

cd ../pr-review-server

echo "✅ Successfully built cbpr for Linux at ./bin/cbpr-linux"
echo ""
echo "Now you can build the Docker image:"
echo "  make build"
