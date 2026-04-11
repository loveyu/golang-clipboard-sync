#!/bin/bash

# Build for Linux
# Usage: ./build_linux.sh [amd64|arm64] [output_dir]
# Default: amd64, current directory

set -e

ARCH="${1:-amd64}"
OUTPUT_DIR="${2:-.}"

case "$ARCH" in
  amd64)
    OUTPUT="clipboard-sync-linux-amd64"
    ;;
  arm64)
    OUTPUT="clipboard-sync-linux-arm64"
    ;;
  *)
    echo "Usage: $0 [amd64|arm64] [output_dir]"
    exit 1
    ;;
esac

echo "Building clipboard-sync for Linux $ARCH..."

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# Clean before build
go clean

BUILD_TIME=$(date '+%Y-%m-%dT%H:%M:%S%z' | tr -d ' ')
GIT_BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
GIT_COMMIT=$(git rev-parse --short=8 HEAD 2>/dev/null || echo "unknown")

GOOS=linux GOARCH=$ARCH go build \
  -ldflags "-X main.version=dev -X main.gitBranch=${GIT_BRANCH} -X main.gitCommit=${GIT_COMMIT} -X main.buildTime=${BUILD_TIME}" \
  -o "$OUTPUT_DIR/$OUTPUT" .

echo "Build complete: $OUTPUT_DIR/$OUTPUT"
ls -lh "$OUTPUT_DIR/$OUTPUT"
