# AGENTS.md — AI Agent Instructions for bot-detector

## Pre-commit checklist

Run these **before every commit**, in order:

```sh
gofmt -w $(gofmt -l .)   # Format ALL files, not just the ones you touched
go vet ./...              # Static analysis
go test ./... -count=1    # Full test suite (no cache)
```

If adding a new CLI flag, config field, or endpoint, also run:

```sh
go build -o bot-detector ./cmd/bot-detector   # Verify binary builds
./bot-detector --check --config-dir testdata   # Validate test config
```

## Testing

- Tests use external test packages (e.g., `package processor_test`), so helpers/types must be exported
- The `internal/testutil` package provides `NewTestProcessor`, `MockBlocker`, etc.
- When adding a method to the `server.Provider` interface, update ALL mock providers:
  - `internal/server/handlers_archive_test.go` (`MockProvider`)
  - `internal/server/server_test.go` (`mockProvider`)
  - `internal/server/handlers_ip_test.go` (`mockIPProvider`)
- Run `go test -race ./...` for race detection (CI does this)

## Dry-run mode

- `--dry-run` processes a static log file and exits
- In multi-website mode, `--log-path` is accepted (not ignored) during dry-run
- `--log-path -` and `--log-path /dev/stdin` read from stdin
- `--chain <name>` filters which chains are evaluated (repeatable flag)
- Dry-run resolves vhost from each log line using the configured log format regex
- For quick testing with the production config:
  ```sh
  ./bot-detector --dry-run \
    --config-dir /path/to/config \
    --log-path access.log \
    --chain MyChain \
    --top-n 10
  ```
- Set `log_level: "critical"` in config for quiet mode (summary only)

## Code conventions

- Small, focused commits — one logical change per commit
- Commit messages: `area: short description` (e.g., `parser: handle empty quoted request`)
- Config fields: add to both `ApplicationConfig` (runtime) and `ApplicationConfigYAML` (parsing), plus the constant default in `constants.go`
- New endpoints: register in `server.go`, add to `allEndpoints` in `handlers_help.go`, document in `docs/API.md`
- New config fields: document in `docs/Configuration.md`
- New CLI flags: document in `README.md` (CLI flags table + relevant sections)

## Project structure

- `cmd/bot-detector/main.go` — entry point, processor initialization, mode dispatch
- `internal/checker/checker.go` — chain evaluation logic (`CheckChains`)
- `internal/config/config.go` — config loading, matcher compilation
- `internal/processor/modes.go` — `DryRunLogProcessor`, `LiveLogTailer`, `MultiLogTailer`
- `internal/logparser/parser.go` — log line processing, parse error recording
- `internal/parser/parser.go` — regex-based log line parsing, `DefaultLogFormatRegex`
- `internal/server/` — HTTP API handlers and routing
- `internal/metrics/` — counters, ring buffers
- `internal/app/types.go` — `Processor` struct (central state)
- `internal/app/providers.go` — `Provider` interface implementations
- `internal/app/configmanager.go` — config reload, `InitializeMetrics`

## CI

- CI runs: `gofmt` check, `golangci-lint`, `go vet`, `go test -race`, `govulncheck`
- `run_checks.sh` mirrors CI locally — run it before pushing
- `govulncheck` failures from stdlib vulns require Go toolchain upgrade, not code changes
