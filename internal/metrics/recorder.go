package metrics

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/logging"
	"github.com/wiruzman/omnia/internal/procio"
)

var processStartedAt = time.Now()

type Process struct {
	CPUUserSeconds   float64 `json:"cpu_user_seconds_total"`
	CPUSystemSeconds float64 `json:"cpu_system_seconds_total"`
	ResidentBytes    uint64  `json:"resident_bytes"`
	FootprintBytes   uint64  `json:"footprint_bytes"`
}

// ProcessPeaks holds observed interval maxima independently of whether the
// latest process sample is available. Nil means no valid sample in the window.
type ProcessPeaks struct {
	ResidentMaxBytes  uint64 `json:"resident_bytes_max"`
	FootprintMaxBytes uint64 `json:"footprint_bytes_max"`
}

type Memory struct {
	HeapBytes      uint64  `json:"heap_bytes"`
	HeapInuseBytes uint64  `json:"heap_inuse_bytes"`
	Goroutines     int     `json:"goroutines"`
	GCCount        uint32  `json:"gc_cycles_total"`
	GCPauseSeconds float64 `json:"gc_pause_seconds_total"`
}

// Record is the entire persisted interface. Only timestamps, numbers, booleans,
// and fixed field names are permitted. Unsupported samples are null, never zero.
type Record struct {
	SchemaVersion       int            `json:"schema_version"`
	Sequence            uint64         `json:"sequence"`
	Timestamp           time.Time      `json:"timestamp"`
	ProcessStartedAt    time.Time      `json:"process_started_at"`
	IntervalSeconds     float64        `json:"interval_seconds"`
	Gap                 bool           `json:"gap"`
	ClockChanged        bool           `json:"clock_changed"`
	CounterReset        bool           `json:"counter_reset"`
	Process             *Process       `json:"process"`
	ProcessPeaks        *ProcessPeaks  `json:"process_peaks"`
	IO                  *procio.DiskIO `json:"io_total"`
	IODelta             *procio.DiskIO `json:"io_delta"`
	IOIntervalStartedAt *time.Time     `json:"io_interval_started_at"`
	Memory              Memory         `json:"go"`
	Work                Work           `json:"work"`
	DatabaseBytes       *int64         `json:"database_bytes"`
	WALBytes            *int64         `json:"wal_bytes"`
	SampleFailures      uint64         `json:"sample_failures_total"`
	WriteFailures       uint64         `json:"write_failures_total"`
	SnapshotsCoalesced  uint64         `json:"snapshots_coalesced_total"`
}

// collector owns sampling and snapshot windows, never sink delivery state.
type collector struct {
	cfg            config.Config
	counters       *Counters
	now            func() time.Time
	sample         func(int) (procio.Sample, error)
	started        time.Time
	lastAt         time.Time
	latest         *procio.Sample
	peaks          *ProcessPeaks
	sampleFailures uint64
	sequence       uint64
	lastSampleOK   bool
	missingSample  bool
}

// Start separates collection from delivery. Each destination gets its own bounded
// queue and worker; a future exporter can be supplied without changing producers
// or the collector. Local history is always included when metrics are enabled.
// Disabled mode creates no files or goroutines and does not start extra sinks.
func Start(cfg config.Config, counters *Counters, logger logging.PrintfLogger, extraSinks ...Sink) func() {
	if !cfg.MetricsEnabled {
		return func() {}
	}
	sinks := append([]Sink{NewJSONLSink(cfg)}, extraSinks...)
	outputs := newFanout(time.Duration(cfg.MetricsIntervalSeconds)*time.Second, logger, sinks...)
	r := newCollector(cfg, counters)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Output queues remain open until the collector has published its final snapshot.
		r.run(ctx, func(record Record) { _ = outputs.Publish(record) })
		closeCtx, cancel := context.WithTimeout(context.Background(), sinkTimeout)
		defer cancel()
		if err := outputs.Close(closeCtx); err != nil && logger != nil {
			logger.Printf("metrics shutdown drain failed")
		}
	}()
	return func() { cancel(); <-done }
}

func newCollector(cfg config.Config, counters *Counters) *collector {
	return &collector{cfg: cfg, counters: counters,
		now: time.Now, sample: procio.CurrentSample, started: processStartedAt}
}

func (r *collector) run(ctx context.Context, publish func(Record)) {
	sampleTicker := time.NewTicker(15 * time.Second)
	writeTicker := time.NewTicker(time.Duration(r.cfg.MetricsIntervalSeconds) * time.Second)
	defer sampleTicker.Stop()
	defer writeTicker.Stop()
	publishSnapshot := func() {
		r.collect()
		publish(r.snapshot())
	}
	publishSnapshot()
	for {
		select {
		case <-ctx.Done():
			publishSnapshot()
			return
		case <-sampleTicker.C:
			r.collect()
		case <-writeTicker.C:
			publishSnapshot()
		}
	}
}

func (r *collector) collect() {
	sample, err := r.sample(os.Getpid())
	r.lastSampleOK = err == nil
	if err != nil {
		r.sampleFailures++
		r.missingSample = true
		return
	}
	r.latest = &sample
	if r.peaks == nil {
		r.peaks = &ProcessPeaks{}
	}
	r.peaks.ResidentMaxBytes = max(r.peaks.ResidentMaxBytes, sample.ResidentBytes)
	r.peaks.FootprintMaxBytes = max(r.peaks.FootprintMaxBytes, sample.FootprintBytes)
}

func (r *collector) snapshot() Record {
	now := r.now()
	start := r.lastAt
	if start.IsZero() {
		start = r.started
	}
	wallSeconds := float64(now.UnixNano()-start.UnixNano()) / 1e9
	monotonicSeconds := now.Sub(start).Seconds()
	work := r.counters.take()
	record := Record{SchemaVersion: 1, Sequence: r.sequence + 1, Timestamp: now.UTC(), ProcessStartedAt: r.started.UTC(),
		Work:           work,
		ClockChanged:   wallSeconds < 0 || wallSeconds-monotonicSeconds < -2,
		Gap:            r.missingSample || wallSeconds > float64(r.cfg.MetricsIntervalSeconds)*1.5,
		SampleFailures: r.sampleFailures,
		DatabaseBytes:  sizeOf(r.cfg.IndexDBPath), WALBytes: sizeOf(r.cfg.IndexDBPath + "-wal")}
	record.Gap = record.Gap || record.ClockChanged
	if r.latest != nil && r.lastSampleOK {
		sample := *r.latest
		record.Process = &Process{sample.CPUUserSeconds, sample.CPUSystemSeconds,
			sample.ResidentBytes, sample.FootprintBytes}
		record.IO = &sample.DiskIO
	}
	record.ProcessPeaks = r.peaks
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	record.Memory = Memory{mem.HeapAlloc, mem.HeapInuse, runtime.NumGoroutine(), mem.NumGC, float64(mem.PauseTotalNs) / 1e9}
	// Delivery workers independently track successful writes. Sampling windows
	// advance on collection, regardless of the speed or health of destinations.
	r.lastAt = now
	r.sequence++
	r.peaks = nil
	r.missingSample = false
	return record
}

func sizeOf(path string) *int64 {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	size := info.Size()
	return &size
}
