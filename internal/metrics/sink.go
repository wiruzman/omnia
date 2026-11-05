package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/logging"
	"github.com/wiruzman/omnia/internal/procio"
)

// Sink consumes numeric snapshots. Write and Close must honor context cancellation.
// Calls are serialized per sink; distinct sinks run independently. Delivery is
// best-effort, not exactly-once. ProcessStartedAt and Sequence identify a snapshot.
type Sink interface {
	Write(context.Context, Record) error
	Close(context.Context) error
}

// JSONLSink is the local-history adapter. Constructing it performs no I/O.
type JSONLSink struct{ history history }

func NewJSONLSink(cfg config.Config) *JSONLSink {
	return &JSONLSink{history: history{dir: cfg.MetricsDir(), days: cfg.MetricsRetentionDays, maxBytes: cfg.MetricsMaxBytes}}
}

func (s *JSONLSink) Write(ctx context.Context, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.history.append(record.Timestamp, append(data, '\n'))
}

func (s *JSONLSink) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.history.sync()
}

// delivery keeps success baselines per destination. A slow/failing exporter
// cannot change another destination's deltas, interval peaks, or failure counts.
type delivery struct {
	sink     Sink
	interval time.Duration
	lastAt   time.Time
	lastIO   *Record
	failed   *Record
	failures uint64
}

func (d *delivery) write(ctx context.Context, input Record) error {
	r := cloneRecord(input)
	if d.failed != nil {
		r = mergeSnapshots(*d.failed, r)
	}
	start := d.lastAt
	if start.IsZero() {
		start = r.ProcessStartedAt
	}
	seconds := r.Timestamp.Sub(start).Seconds()
	r.IntervalSeconds = max(0, seconds)
	r.Gap = r.Gap || seconds > d.interval.Seconds()*1.5 || seconds < 0
	r.ClockChanged = r.ClockChanged || seconds < 0
	r.WriteFailures = d.failures
	r.IODelta = nil
	r.IOIntervalStartedAt = nil
	r.CounterReset = false
	if r.IO != nil {
		previous := Record{ProcessStartedAt: r.ProcessStartedAt, Timestamp: r.ProcessStartedAt}
		if d.lastIO != nil {
			previous = *d.lastIO
		}
		read, written, logical := uint64(0), uint64(0), uint64(0)
		if previous.IO != nil {
			read, written, logical = previous.IO.BytesRead, previous.IO.BytesWritten, previous.IO.LogicalBytesWritten
		}
		r.CounterReset = r.IO.BytesRead < read || r.IO.BytesWritten < written || r.IO.LogicalBytesWritten < logical
		if previous.Process != nil && r.Process != nil {
			r.CounterReset = r.CounterReset || r.Process.CPUUserSeconds < previous.Process.CPUUserSeconds || r.Process.CPUSystemSeconds < previous.Process.CPUSystemSeconds
		}
		if !r.CounterReset {
			ioStart := previous.Timestamp
			r.IOIntervalStartedAt = &ioStart
			r.IODelta = &procio.DiskIO{BytesRead: r.IO.BytesRead - read, BytesWritten: r.IO.BytesWritten - written, LogicalBytesWritten: r.IO.LogicalBytesWritten - logical}
			r.Gap = r.Gap || (!d.lastAt.IsZero() && previous.Timestamp.Before(d.lastAt))
		}
	}
	// Give an adapter its own copy; mutation cannot corrupt this destination's
	// baseline or any other destination's snapshot.
	if err := d.sink.Write(ctx, cloneRecord(r)); err != nil {
		d.failures++
		d.failed = &r
		return err
	}
	d.lastAt = r.Timestamp
	if r.IO != nil {
		copy := cloneRecord(r)
		d.lastIO = &copy
	}
	d.failed = nil
	return nil
}

func cloneRecord(r Record) Record {
	if r.ProcessPeaks != nil {
		value := *r.ProcessPeaks
		r.ProcessPeaks = &value
	}
	if r.Process != nil {
		value := *r.Process
		r.Process = &value
	}
	if r.IO != nil {
		value := *r.IO
		r.IO = &value
	}
	if r.IODelta != nil {
		value := *r.IODelta
		r.IODelta = &value
	}
	if r.IOIntervalStartedAt != nil {
		value := *r.IOIntervalStartedAt
		r.IOIntervalStartedAt = &value
	}
	if r.DatabaseBytes != nil {
		value := *r.DatabaseBytes
		r.DatabaseBytes = &value
	}
	if r.WALBytes != nil {
		value := *r.WALBytes
		r.WALBytes = &value
	}
	return r
}

// Keep the newest cumulative values while preserving peaks and discontinuities
// from snapshots being coalesced or carried forward after a delivery failure.
func mergeSnapshots(older, newer Record) Record {
	newer.Work.PendingMax = max(older.Work.PendingMax, newer.Work.PendingMax)
	newer.Work.FlushMaxSeconds = max(older.Work.FlushMaxSeconds, newer.Work.FlushMaxSeconds)
	newer.Work.PreviewMaxSeconds = max(older.Work.PreviewMaxSeconds, newer.Work.PreviewMaxSeconds)
	newer.Work.ScanMaxSeconds = max(older.Work.ScanMaxSeconds, newer.Work.ScanMaxSeconds)
	if older.ProcessPeaks != nil {
		peaks := *older.ProcessPeaks
		if newer.ProcessPeaks != nil {
			peaks.ResidentMaxBytes = max(peaks.ResidentMaxBytes, newer.ProcessPeaks.ResidentMaxBytes)
			peaks.FootprintMaxBytes = max(peaks.FootprintMaxBytes, newer.ProcessPeaks.FootprintMaxBytes)
		}
		newer.ProcessPeaks = &peaks
	}
	newer.Gap = newer.Gap || older.Gap
	newer.ClockChanged = newer.ClockChanged || older.ClockChanged
	return newer
}

const sinkTimeout = 5 * time.Second

var errSinkClosed = errors.New("metrics sink closed")

// One snapshot can be in flight and one pending. If delivery falls behind,
// replace the pending snapshot with newer cumulative values and merged peaks.
type sinkWorker struct {
	mu        sync.Mutex
	pending   chan Record
	closed    bool
	coalesced uint64
	done      chan struct{}
}

func (w *sinkWorker) publish(record Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errSinkClosed
	}
	record = cloneRecord(record)
	select {
	case previous := <-w.pending:
		record = mergeSnapshots(previous, record)
		record.Gap = true
		w.coalesced++
	default:
	}
	record.SnapshotsCoalesced = w.coalesced
	// Only publishers send, under this mutex; the channel has room after the
	// nonblocking receive above. The worker can only free additional room.
	w.pending <- record
	return nil
}

func (w *sinkWorker) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.pending)
	}
}

type fanout struct {
	workers []*sinkWorker
	cancel  context.CancelFunc
}

func newFanout(interval time.Duration, logger logging.PrintfLogger, sinks ...Sink) *fanout {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fanout{cancel: cancel}
	for _, sink := range sinks {
		worker := &sinkWorker{pending: make(chan Record, 1), done: make(chan struct{})}
		f.workers = append(f.workers, worker)
		go func() {
			defer close(worker.done)
			d := delivery{sink: sink, interval: interval}
			defer func() {
				closeCtx, cancel := context.WithTimeout(ctx, sinkTimeout)
				defer cancel()
				if err := sink.Close(closeCtx); err != nil && logger != nil {
					logger.Printf("metrics sink close failed")
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case record, ok := <-worker.pending:
					if !ok {
						return
					}
					writeCtx, cancel := context.WithTimeout(ctx, sinkTimeout)
					err := d.write(writeCtx, record)
					cancel()
					if err != nil && logger != nil && (d.failures == 1 || d.failures%60 == 0) {
						logger.Printf("metrics sink write failed | failures=%d", d.failures)
					}
				}
			}
		}()
	}
	return f
}

func (f *fanout) Publish(r Record) error {
	for _, worker := range f.workers {
		if err := worker.publish(r); err != nil {
			return err
		}
	}
	return nil
}

func (f *fanout) Close(ctx context.Context) error {
	defer f.cancel()
	for _, worker := range f.workers {
		worker.close()
	}
	for _, worker := range f.workers {
		select {
		case <-worker.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
