#!/bin/bash
set -e

# Get git commit and build time
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u '+%Y-%m-%d_%H:%M:%S')

echo "Building Docker image with:"
echo "  Git Commit: $GIT_COMMIT"
echo "  Build Time: $BUILD_TIME"

# Build the Docker image
docker build \
  --build-arg GIT_COMMIT="$GIT_COMMIT" \
  --build-arg BUILD_TIME="$BUILD_TIME" \
  -t bot-detector:latest \
  -t "bot-detector:$GIT_COMMIT" \
  .

echo "Build complete!"
echo "Tagged as: bot-detector:latest and bot-detector:$GIT_COMMIT"
