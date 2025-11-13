# Contributing

This document provides instructions for setting up a development environment for this project.

## Development Environment

### Go Version

This project uses Go `1.25`. Please make sure you have this version installed.

### GolangCI-Lint

We use `golangci-lint` for linting. The version used in our CI is `v2.6.1`.

To install it, you can use the following command:

```bash
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.6.1
```

### Running Checks

A shell script is provided to run all the necessary checks.

```bash
./run_checks.sh
```
