# Omnia Search TUI (macOS)

Omnia is a keyboard-first terminal file search app for macOS. It keeps a local SQLite FTS5 trigram index and gives you fast interactive search over indexed paths while the background daemon owns indexing.

## What it does

- Shows indexed filesystem entries in a table: name, path, type, size, created, modified.
- Supports live search while typing.
- Supports sorting by name, path, size, created, modified in ascending or descending order.
- Opens files or folders with the default macOS handler.
- Reveals items in Finder.
- Copies selected path to clipboard.
- Deletes selected items by moving them to Trash (with confirmation).
- Persists index and settings between runs.

## Architecture at a glance

- TUI: Go + tview/tcell.
- Store: SQLite FTS5 trigram index. Bleve remains available as a legacy backend.
- Scanner/indexer: filesystem walk + batched upserts.
- Daemon: watches configured roots for incremental updates and runs reindex only when triggered manually or when bootstrapping an empty index.

## Requirements

- macOS
- Go 1.25+

## Build

```bash
go mod tidy
go build -o omnia ./cmd/omnia
go build -o omnia-daemon ./cmd/omnia-daemon
```

## Run

Start the daemon first:

```bash
./omnia-daemon
```

Then run the TUI in another terminal:

```bash
./omnia
```

## First-run paths

On first launch, Omnia creates default configuration under:

- ~/.config/omnia-search/config.json

Logs and daemon state are stored under:

- ~/.config/omnia-search/omnia.log
- ~/.config/omnia-search/daemon/

## Search behavior

For non-empty queries, Omnia prioritizes cheaper lookups first to keep typing responsive:

- Prefix match on file name.
- Prefix match on full path.
- For longer queries, contains match on name, then path.

If a query is slow or times out, the app falls back to bounded in-memory filtering so the UI stays responsive.

## Daemon behavior

- The daemon owns indexing work.
- The TUI reads daemon status and triggers reindex requests through a trigger file.
- The daemon applies incremental updates from filesystem events.
- The daemon starts a full reindex automatically when it detects an empty index.

## Keyboard shortcuts

- /: focus search
- Esc: clear search or close details
- Arrow Up/Down or j/k: move rows
- Arrow Left/Right: move columns
- Enter: open selected file or folder
- Space: toggle details panel
- s: cycle sort column
- Shift+s: reverse sort direction
- r: toggle daemon reindex (stop if running, resume if stopped)
- Shift+r: fresh reindex from scratch
- f: reveal in Finder
- d: delete selected (confirm, moves to Trash)
- y: copy selected path
- q: quit

## Configuration

Use config.example.json as a template for ~/.config/omnia-search/config.json.

Key fields:

- include_paths: additional roots to index; your home directory is always added automatically
- exclude_globs: path segments to skip
- index_db_path: index database path (relative values are resolved under ~/.config/omnia-search)
- store_backend: sqlite by default; bleve remains available for existing legacy indexes
- daemon_dir: daemon status, trigger, and log directory (relative values are resolved under ~/.config/omnia-search)
- sort_column: one of name, path, size, created, modified
- sort_direction: ASC or DESC
- max_results: max rows shown per query
- debounce_ms: search debounce
- scan_batch_size: batch size for index upserts
- scan_throttle_every: number of scanned entries between low-power pauses during full reindex
- scan_throttle_ms: pause duration for each low-power scan pause; set to 0 to disable throttling

## Benchmark

Run the synthetic benchmark comparing Bleve with the SQLite FTS5 trigram store:

```bash
go run ./cmd/bench
```

Useful options:

```bash
go run ./cmd/bench -n 200000 -runs 20 -batch 2000 -limit 100
```

## Test

```bash
go test ./...
```

## Limitations

- No NTFS MFT-equivalent metadata source on macOS.
- Initial indexing on large trees can take time.
- File event delivery can miss edge cases, so run a manual reindex when needed (for example Shift+r in the TUI).
