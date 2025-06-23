#!/bin/bash
# Cross-compile script for herd-cache-bench

set -e

echo "Building herd-cache-bench for multiple platforms..."

# Linux AMD64 (most common servers)
echo "Building for Linux AMD64..."
GOOS=linux GOARCH=amd64 go build -o herd-cache-bench-linux-amd64

# Linux ARM64 (newer ARM servers, AWS Graviton)
echo "Building for Linux ARM64..."
GOOS=linux GOARCH=arm64 go build -o herd-cache-bench-linux-arm64

# macOS (for local testing)
echo "Building for macOS..."
go build -o herd-cache-bench-darwin

echo "Done! Built binaries:"
ls -lh herd-cache-bench-*

echo ""
echo "Note: simdjson and sonic will only work on Linux AMD64 with modern CPUs"
echo "jsoniter will work on all platforms"