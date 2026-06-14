# Contributing

Omnia is a macOS-focused Go project. Keep changes small, tested, and aligned with the existing package boundaries.

## Local setup

- Install Go 1.25 or newer.
- Ensure macOS command-line tools and system SQLite are available.
- Run commands from the repository root.

## Checks

Run the full test suite before opening a pull request:

```bash
go test ./...
```

For changes that touch shared packages, build both user-facing commands:

```bash
go build -o /tmp/omnia ./cmd/omnia
go build -o /tmp/omnia-daemon ./cmd/omnia-daemon
```

For benchmark changes, also build the benchmark command:

```bash
go build -o /tmp/omnia-bench ./cmd/bench
```

## Public data hygiene

- Do not commit local configuration, logs, SQLite indexes, benchmark output, or environment files.
- Use `config.example.json` for documented configuration defaults.
- Avoid adding real local filesystem paths, user names, credentials, or private machine details to fixtures.
