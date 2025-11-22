# Contributing

This document provides instructions for setting up a development environment and contributing to the bot-detector project.

## Development Environment

### Go Version

This project uses Go `1.25`. Please make sure you have this version installed.

### GolangCI-Lint

We use `golangci-lint` for linting. The version used in our CI is `v2.6.1`.

To install it:

```bash
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.6.1
```

## Running Checks

Before submitting a pull request, run the provided check script to ensure your code meets project standards:

```bash
./run_checks.sh
```

### What the Script Does

The `run_checks.sh` script performs the following checks in sequence:

1. **go vet** - Examines Go source code and reports suspicious constructs
2. **golangci-lint** - Runs multiple linters to catch common issues and enforce code style
3. **go test -race** - Runs all tests with race detection enabled (30s timeout)
4. **go build** - Compiles the application to ensure it builds successfully
5. **gofmt** - Formats all Go source files according to Go standards
6. **Dry-run test** - Executes the bot-detector against test data to verify functionality

The script will exit immediately if any check fails. All checks must pass before code can be merged.

## Code Style

- Follow standard Go conventions and idioms
- Run `gofmt` on all code (included in `run_checks.sh`)
- Ensure all tests pass with race detection enabled
- Add tests for new functionality
- Keep functions focused and concise
