#!/bin/bash

# Exit immediately if a command exits with a non-zero status.
set -e

# Run go vet
echo "Running go vet..."
go vet ./...

# Run go test
echo "Running go test..."
go test ./...

# Run golangci-lint
echo "Running golangci-lint..."
golangci-lint run

# Run go build
echo "Running go build..."
go build

# Run gofmt
echo "Running gofmt..."
find . -name "*.go" -print0 | xargs -0 gofmt -w

echo "All checks passed!"
