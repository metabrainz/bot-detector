#!/bin/bash
set -e

# Accept tag as first argument, default to "latest"
TAG=${1:-latest}

# Get git commit for tagging
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")

echo "Building Docker image..."
echo "  Tag: $TAG"
echo "  Git Commit: $GIT_COMMIT (will be auto-embedded by Go build)"

# Build the Docker image
# Go 1.18+ automatically embeds VCS info when .git directory is present
docker build \
  -t "bot-detector:$TAG" \
  -t "bot-detector:$GIT_COMMIT" \
  .

echo "Build complete!"
echo "Tagged as: bot-detector:$TAG and bot-detector:$GIT_COMMIT"
