#!/bin/bash

# Exit immediately if a command exits with a non-zero status.
set -ex

# Run go vet
echo "Running go vet..."
go vet ./...

# Run golangci-lint
echo "Running golangci-lint..."
golangci-lint run

# Run go test -race
echo "Running go test -race..."
go test -race ./...

# Run go build
echo "Running go build..."
go build

# Run gofmt
echo "Running gofmt..."
find . -name "*.go" -print0 | xargs -0 gofmt -w

# Run bot-detector --dry-run with testdata
echo "Running bot-detector --dry-run with testdata..."
./bot-detector --dry-run --config testdata/config.yaml --log-path testdata/test_access.log --top-n 10

echo "All checks passed!"
