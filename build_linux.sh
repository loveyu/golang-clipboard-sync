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

GOOS=linux GOARCH=$ARCH go build -o "$OUTPUT_DIR/$OUTPUT" .

echo "Build complete: $OUTPUT_DIR/$OUTPUT"
ls -lh "$OUTPUT_DIR/$OUTPUT"
