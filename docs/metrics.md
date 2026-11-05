# Local performance history

Omnia can record aggregate daemon performance measurements without recording
which files or projects were involved. It does not require a monitoring server.
This is independent of diagnostic logging, including when the log level is `warn`.

## Enable collection

Add these fields to `~/.config/omnia-search/config.json` and restart a daemon built
with this feature:

```json
{
  "metrics_enabled": true,
  "metrics_interval_seconds": 60,
  "metrics_retention_days": 180,
  "metrics_max_bytes": 1073741824
}
```

Collection is **opt-in**. Old configurations remain disabled. Invalid/nonpositive
retention or size limits fall back to the defaults above; recording intervals
below 15 seconds (or overflowing Go's timer duration) fall back to 60 seconds.
Disabling collection stops new records and automatic cleanup, leaving previously
recorded history in place.

Files live under `<daemon_dir>/metrics/`, normally
`~/.config/omnia-search/daemon/metrics/`. New directories use mode 0700 and files
0600. No network listener or data shipping is enabled. This feature concerns the
metrics files only: existing diagnostic logs and the search database can contain
paths and must not be treated as sanitized metrics.

## What is recorded

Each JSON line has a fixed numeric schema, version `1`. Its only string values are
timestamps. There are no filenames, paths, extensions, file identifiers/hashes,
query text, user/host names, process arguments, arbitrary labels, or raw error
messages. Counts and timestamps still reveal aggregate activity patterns.

| Group | Measurements |
| --- | --- |
| Process | Cumulative user/system CPU seconds, resident and physical-footprint bytes, sampled memory maxima |
| I/O | OS-attributed disk bytes read/written, logical bytes written, interval deltas |
| Go | Heap allocation/in-use bytes, goroutines, cumulative GC cycles and pause seconds |
| Index | Indexed entry count, database and WAL sizes, pending entries and interval maximum |
| Events | Cumulative events received/accepted/ignored |
| Incremental work | Flush count, inserted/updated/deleted/unchanged/skipped entries, failed entries, cumulative and maximum flush duration |
| Full indexing | Scan runs, active scans, scanned entries, failures, cumulative and maximum completed-scan duration |
| Preview | Cache publication attempts/failures, cumulative and maximum publication duration |
| Recorder | OS-sample and per-sink persistence failure counts, coalesced snapshots, missing-period/reset indicators |

The complete field names and types are defined in `internal/metrics/recorder.go`
and `internal/metrics/counters.go`. Suffixes `_bytes` and `_seconds` specify units;
`_total` fields are cumulative within a daemon process. Memory and duration maxima
cover the interval since the last successfully persisted record. Scan duration
is added when a scan finishes, including interrupted scans; ordinary cancellation
is not counted as a scan failure. Unchanged/skipped paths are separate counters.

Process memory is sampled every 15 seconds and summarized at the recording
interval. `process_peaks` holds observed maxima separately from the current
`process` sample, so a failed final sample does not discard earlier observations.
It is `null` only when no valid sample was observed in the window. Maxima are
sampled peaks, not guarantees that every subsecond spike was observed.
Go memory is read at record time. Work durations are measured directly;
no per-file records or timestamps are retained. The entry count reuses the
daemon's maintained estimate instead of issuing full database counts for metrics.

On macOS, process measurements come from `proc_pid_rusage`. Disk writes are OS
accounting, not a measurement of physical SSD NAND wear. They include SQLite,
status/log writes, and the metrics recorder's own overhead. Unsupported/failed
measurements and unavailable file sizes are `null`, not fabricated zeros.

## Time, restarts, and daily totals

Records include UTC `timestamp`, `process_started_at`, and a process-local
`sequence`. Use `(process_started_at, sequence)` for ordering/deduplication;
wall-clock timestamps can move backwards. Startup and graceful shutdown each
produce a snapshot in addition to periodic snapshots. Sequence gaps can occur
after failed delivery or queue coalescing; a sequence does not imply durability.

`io_total` contains process-lifetime counters. `io_delta` is their difference from
the previous successfully delivered I/O sample in the same process, independently
for each destination. The first
sample includes process startup I/O. `io_interval_started_at` specifies the actual
start of that difference, which can precede the previous record if an OS sample
was unavailable. Never subtract counters across processes. A counter decrease
sets `counter_reset` and makes that record's `io_delta` null; the next valid sample
establishes a new difference from the reset baseline.

To calculate approximate daily writes, stream complete records, sum
`io_delta.written_bytes`, and group by the end-of-interval timestamp in the desired
timezone. For example, this produces **UTC** dates and GiB totals from complete
JSON-line records (requires `jq`):

```sh
find "$HOME/.config/omnia-search/daemon/metrics" -name 'metrics-v1-*.jsonl' -type f -exec jq -c 'select(.io_delta != null) | {day: (.timestamp[0:10]), bytes: .io_delta.written_bytes}' {} + |
  jq -s 'group_by(.day) | map({day: .[0].day, written_gib: (map(.bytes) | add / 1073741824)})'
```

A minute that crosses midnight is attributed to its end date by that example.
If finer calendar attribution is needed, split its bytes proportionally across
`io_interval_started_at` and `timestamp` and label those portions as estimates.
For local dates use a timezone-aware importer; do not assume every local day is
24 hours during daylight-saving transitions. This is sampled OS accounting,
not exact per-calendar-day tracing.

`gap` flags long recording intervals, failed samples, or a backward clock.
`clock_changed` flags backward wall-clock discontinuities. CPU utilization can
be calculated from consecutive valid process CPU totals divided by elapsed
seconds; 100% means one fully occupied core. Do not interpolate activity across
sleep/downtime or treat missing samples as zero. A process crash can lose the last
interval's unsaved data. `ReadRecords` streams complete lines and ignores a
truncated final line; the writer removes that incomplete tail before appending
after a restart. Invalid complete lines and unknown schema versions are errors.

## Retention and space

Files are named `metrics-v1-YYYY-MM-DD-NNNN.jsonl`, rotated by UTC day, and split
at 8 MiB if necessary. Each write prunes owned files whose entire UTC day is older
than the retention window, so time-based retention can include up to one extra
partial day. A byte cap takes precedence and deletes oldest complete files first.
Only strictly matching regular files in this directory are eligible for deletion;
unrelated files and symlinks are excluded. A cap too small for one record prevents
recording and increments the persistence failure counter.

Representative schema-v1 records are about **1.5 KiB**. At one record per minute:

| Retention | Estimated payload at 1,551 bytes/record | Planning range at 1–2 KiB/record |
| --- | --- | --- |
| 30 days | 64 MiB | 42–84 MiB |
| 90 days | 192 MiB | 127–253 MiB |
| 180 days | 383 MiB | 253–506 MiB |

Record size varies with the number of digits in counters. Startup/shutdown records,
filesystem metadata/allocation, and extra partial-day retention add overhead.
The default 1 GiB limit leaves headroom. The cap measures file lengths, not SSD
write traffic or filesystem metadata. A 15-second recording interval costs about
four times as much; five-minute records cost about one fifth. No compression is
used in this version. Retained bytes and bytes written are different quantities.

The collector and destination writers run independently. Each destination has one
worker, one pending snapshot, and at most one in flight. The
local writer uses append writes rather than rewriting old history and does not
open or query the index. No per-record fsync is requested. Recorder paths are
excluded from incremental indexing and full scans by the daemon-directory
exclusion.

## Collection and exporter boundary

```text
Indexing events -> numeric counters -> timed collector
                                       |-> bounded queue -> JSONL history
                                       |-> bounded queue -> future exporter
```

Event handlers only update in-memory counters. The collector reads those counters
and process samples and publishes owned numeric snapshots. It advances sampling
windows on capture and has no sink, write-failure, or shutdown responsibilities.
Workers alone own delivery success baselines and failure recovery. Serialization,
delivery I/O, and history retention are outside the indexing execution path.

The `Sink` interface in `internal/metrics/sink.go` has two methods:

```go
Write(context.Context, Record) error
Close(context.Context) error
```

`JSONLSink` is the initial adapter. `metrics.Start` accepts optional extra sinks
alongside local history. Each receives the same source snapshot sequence through
its own worker; the collector and event producers do not change when an exporter
is added. Disabled metrics do not start any sinks. Adapter calls are serialized
per destination, with independent snapshot copies and success baselines. A sink
cannot mutate another sink's records or stall the collector through delivery I/O.

Each worker uses a single-slot Go channel. If a snapshot is still pending when a
newer one arrives, replace it with the newer cumulative counters and merge their
interval maxima. No custom ring buffer or backlog is maintained. Intermediate
time resolution is lost under backpressure; `snapshots_coalesced_total` and `gap`
make that visible. Producers never wait for destination I/O, and pending memory
cannot grow. Even startup and immediate shutdown snapshots can be combined if
the worker has not yet consumed the first one; the final sequence and totals are
retained.

Failed writes carry peaks forward and preserve that destination's last successful
I/O baseline. The next successful record covers the intervening cumulative bytes.
`write_failures_total` is specific to that destination; a failed exporter cannot
alter local-history deltas. Delivery is best-effort, not exactly-once, and the
queue is in memory. A process crash can lose queued samples and the unfinished
interval. Counters cannot recover that process's final unobserved activity.

Each delivery attempt has a five-second context deadline. Orderly shutdown
publishes a final snapshot, seals queues, and gives all workers a shared
five-second budget to drain and close. The local sink syncs its last file on close.
After that budget expires, remaining delivery is canceled so an exporter cannot
hold daemon shutdown indefinitely. Adapters must honor context cancellation;
ordinary filesystem calls cannot be forcibly interrupted, though waiting for
workers is bounded. Delivery/close warnings never include raw sink errors or
destination identities.

## Grafana later

The numeric `Record`/`Sink` boundary is ready for a later Prometheus or OTLP
adapter, without moving collection or changing event handlers. The versioned
JSON Lines schema and `ReadRecords` reader also provide a source for historical
import. Live Prometheus collection and
Grafana dashboards are a separate integration; enabling this feature does not
automatically backfill Prometheus. For Loki ingestion, ship only these metrics
files, use a small fixed label set, and keep timestamps/counter values as record
fields rather than dynamic labels. Configure the destination's historical-data
acceptance and retention explicitly before importing old records.

## Verification

`go test ./...` covers privacy/schema constraints, counter resets and restarts,
UTC rotation, sleep/backward-clock gaps, failed writes/samples, truncated lines,
retention windows, the storage cap, and metrics-path exclusion. Run
`go test -race ./internal/metrics ./internal/daemon ./internal/indexer` for
concurrent collection and daemon integration.

Sink tests additionally cover stalled/failed destination isolation, independent
I/O baselines, queue coalescing and peak conservation, adapter mutation, and
deadline-bounded shutdown.

`go test ./internal/metrics -run TestRecorderMeasuredIOOverhead -v` measures an
accelerated 60-record collection/local-write workload, including OS-attributed
writes. It executes those stages synchronously for deterministic accounting;
asynchronous scheduling overhead is not included. This is not a claim about
real-time minute-spaced physical writes.
`go test ./internal/metrics -run '^$' -bench BenchmarkRecorderAppend -benchtime=50x -benchmem`
also includes directory scanning with 180 days of retained files.
