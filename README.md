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
- Shows daemon indexing status and per-include-path scan progress.
- Uses a startup preview cache so the initial empty search view can warm quickly.
- Persists index and settings between runs.

## Architecture at a glance

- TUI: Go + tview/tcell.
- Store: SQLite FTS5 trigram index.
- Scanner/indexer: filesystem walk + batched upserts, per-root progress, and resumable full reindex.
- Daemon: watches configured roots for incremental updates, publishes startup preview snapshots, and runs reindex only when triggered manually or when bootstrapping an empty index.

## Requirements

- macOS
- Go 1.25+

## Build

```bash
go mod tidy
go build -o omnia ./cmd/omnia
go build -o omnia-daemon ./cmd/omnia-daemon
```

## Install from source

```bash
go install github.com/wiruzman/omnia/cmd/omnia@latest
go install github.com/wiruzman/omnia/cmd/omnia-daemon@latest
```

## Install with Homebrew

```bash
brew tap wiruzman/tap
brew install omnia
```

Start the daemon as a Homebrew service:

```bash
brew services start wiruzman/tap/omnia
```

Stop the daemon service:

```bash
brew services stop wiruzman/tap/omnia
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

Check installed versions:

```bash
omnia --version
omnia-daemon --version
```

## First-run paths

On first launch, Omnia creates default configuration under:

- ~/.config/omnia-search/config.json

Logs and daemon state are stored under:

- ~/.config/omnia-search/omnia.log
- ~/.config/omnia-search/daemon/
- ~/.config/omnia-search/daemon/daemon.log

## Search behavior

For non-empty queries, Omnia prioritizes cheaper lookups first to keep typing responsive:

- Plain filename terms: prefix match first, then filename contains for terms of at least 3 characters.
- Short plain terms: 1-2 characters stay prefix-only; 3 characters search filenames but skip broad path contains.
- Longer plain terms: filename matches first; path contains only runs when filename matching finds nothing.
- Slash/path-like terms: path prefix or path contains, skipping filename scans.
- Multi-word terms: exact phrase contains first, then all-term matching so separated words can still match.

If a live query is slow or times out, the app shows no results for that query instead of keeping stale rows or running extra fallback scans.

## Daemon behavior

- The daemon owns indexing work.
- The TUI reads daemon status and triggers reindex requests through a trigger file.
- The daemon applies incremental updates from filesystem events.
- The daemon starts a full reindex automatically when it detects an empty index.
- The daemon tracks per-include-path scan progress for the TUI progress view.
- Interrupted full reindexes can resume from saved daemon state; fresh reindex clears that resume state and starts from scratch.
- The daemon publishes a startup preview cache after indexing snapshots so the TUI can show initial empty-query results quickly.
- The daemon writes JSON-line logs with severity, pid, and component fields, and rotates them by size for service-mode troubleshooting.

Incremental indexing keeps the three-second quiet-period debounce, with a
ten-second maximum scheduling delay when indexing is available. Each incremental
transaction processes at most 128 indexed-entry changes; delivery and pending paths are
bounded. Overflow requests a full reconciliation instead of silently losing
changes. Directory creation and moves discover only their affected subtree. The
native macOS watcher keeps no lifetime history of observed paths.

Full scans refresh a separate compact scan-marker table for unchanged entries,
avoiding writes to wide metadata rows and search indexes. Entry changes and their
markers commit atomically; deleting an entry also deletes its marker. A static
root index avoids index rewrites when only the scan generation changes.
Stale-entry cleanup uses an entry-ID cursor, honors cancellation, and commits at
most 16,384 rows per batch. Existing entries beneath unreadable paths are preserved
using the same scan marker, including across interruption/resume; intentionally
excluded or genuinely missing paths remain eligible for cleanup.

Full scans default to 16,384 entries per transaction. Explicit `scan_batch_size`
values are honored, including existing configurations set to 1,000. Resume cursors
are saved only after database commits, not UI progress updates. Legacy checkpoint
files without a committed cursor restart with a fresh scan ID for safety.

Existing indexes migrate automatically on the next writable open. Migration is
transactional and preserves entry row IDs, scan generations, search indexes, and
saved resume cursors. Removing the old marker column rewrites the entry table
once, so allow temporary disk/WAL headroom during that first startup. Indexes
already using compact markers (0.1.5) only replace the generation-dependent
marker index; their entry/FTS tables and scan generations are not rewritten.
Schema work uses file-backed temporary storage to avoid retaining large native
allocations; runtime queries retain their existing in-memory temporary storage.
Subsequent opens do not repeat the migration. Older writers cannot use the
migrated schema; retain a pre-upgrade index backup if a writer downgrade is
needed. The read-only CLI can read either schema without migrating it.

Coalesced subtree changes, root changes, and volume mount/unmount notifications
recover only the affected configured scope through the existing bounded pending
work. Cached descendants are checked too, so missed deletions are repaired while
inaccessible entries are retained. Actual event loss or exhausted delivery,
pending, or discovery budgets still require a full reconciliation. A new loss
during a full scan retains a follow-up request rather than silently dropping it.

## Optional performance history

Set `metrics_enabled` to `true` in your configuration to record aggregate performance
metrics locally. Collection is disabled by default and independent of the daemon
log level. The defaults are one record per minute, 180-day retention, and a 1 GiB
cap. Records contain no filenames, paths, queries, file hashes, or raw errors.
The built-in sink writes locally; separate bounded sink queues prepare for future
exporters, with no network exporter enabled today. See [metrics history](docs/metrics.md) for the
schema, storage estimates, daily totals, and future Grafana ingestion.

## Keyboard shortcuts

- Shift+?: show/hide shortcuts
- Shift+:: focus search
- Esc: clear search or close details
- Arrow Up/Down or j/k: move rows
- Arrow Left/Right: scroll columns
- Shift+Arrow Left/Right: select column
- Enter: open selected file or folder
- Space: toggle details panel
- s: cycle sort column
- Shift+s: reverse sort direction
- p: toggle include path progress view
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
- daemon_dir: daemon status, trigger, and log directory (relative values are resolved under ~/.config/omnia-search)
- daemon_log_file: daemon log file path; relative values are resolved under daemon_dir
- daemon_log_level: minimum daemon log level: debug, info, warn, or error
- daemon_log_max_bytes: rotate daemon logs after this many bytes
- daemon_log_backups: number of rotated daemon log files to retain
- metrics_enabled: opt in to local numeric performance history; defaults to false
- metrics_interval_seconds: recording interval, minimum 15 seconds; defaults to 60
- metrics_retention_days: history retention; defaults to 180 days
- metrics_max_bytes: maximum metrics payload on disk; defaults to 1073741824 (1 GiB)
- sort_column: one of name, path, size, created, modified
- sort_direction: ASC or DESC
- max_results: max rows shown per query
- debounce_ms: search debounce
- scan_batch_size: batch size for index upserts
- scan_throttle_every: number of scanned entries between low-power pauses during full reindex
- scan_throttle_ms: pause duration for each low-power scan pause; set to 0 to disable throttling
- skip_package_contents: index macOS package directories such as .app and .framework, but skip their internal contents

## Benchmark

Run the synthetic benchmark for the SQLite FTS5 trigram store:

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

## Contributing and security

Issues and pull requests are welcome. See CONTRIBUTING.md for local checks and SECURITY.md for vulnerability reporting guidance.

## License

Omnia is released under the MIT License. See LICENSE for details.

## Limitations

- No NTFS MFT-equivalent metadata source on macOS.
- Initial indexing on large trees can take time.
- File event delivery can miss edge cases, so run a manual reindex when needed (for example Shift+r in the TUI).
