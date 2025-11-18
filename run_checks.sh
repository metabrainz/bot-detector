#!/bin/bash

# Exit immediately if a command exits with a non-zero status.
set -e

# Run go vet
echo "Running go vet..."
go vet ./...

# Run golangci-lint
echo "Running golangci-lint..."
golangci-lint run

# Run go test -race
echo 'Running go test -race...'
timeout 30s go test -race -v ./... 2>&1 | awk '/^FAIL/ || /--- FAIL:/ {print}'
GO_TEST_STATUS=${PIPESTATUS[0]}
if [ $GO_TEST_STATUS -ne 0 ]; then
    echo "Tests failed. Exiting with status $GO_TEST_STATUS."
    exit $GO_TEST_STATUS
else
    echo "All tests passed."
fi

# Run go build
echo "Running go build..."
go build -o bot-detector ./cmd/bot-detector

# Run gofmt
echo "Running gofmt..."
find . -name "*.go" -print0 | xargs -0 gofmt -w

# Run bot-detector --dry-run with testdata
echo "Running bot-detector --dry-run with testdata..."
./bot-detector --dry-run --config-dir testdata --log-path testdata/test_access.log --top-n 10

echo "All checks passed!"
