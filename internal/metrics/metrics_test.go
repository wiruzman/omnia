package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/logging"
	"github.com/wiruzman/omnia/internal/procio"
)

// Exercise collection and delivery separately, with deterministic synchronous
// writes for failure tests rather than depending on worker scheduling.
type recordingFixture struct {
	*collector
	sink     *JSONLSink
	delivery delivery
}

func (r *recordingFixture) write() error { return r.delivery.write(context.Background(), r.snapshot()) }

func testRecorder(t *testing.T) (*recordingFixture, *time.Time, *procio.Sample) {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DaemonDir = filepath.Join(t.TempDir(), "sensitive-user-and-project")
	cfg.IndexDBPath = filepath.Join(t.TempDir(), "private-document-index.sqlite")
	if err := os.WriteFile(cfg.IndexDBPath, make([]byte, 20), 0o600); err != nil {
		t.Fatal(err)
	}
	r := localRecorder(cfg, &Counters{})
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	r.started = now.Add(-time.Minute)
	r.now = func() time.Time { return now }
	sample := procio.Sample{DiskIO: procio.DiskIO{BytesRead: 100, BytesWritten: 200, LogicalBytesWritten: 150}, CPUUserSeconds: 3, ResidentBytes: 40, FootprintBytes: 50}
	r.sample = func(int) (procio.Sample, error) { return sample, nil }
	return r, &now, &sample
}

func records(t *testing.T, dir string) []Record {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "metrics-v1-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var result []Record
	for _, name := range names {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		err = ReadRecords(f, func(r Record) error { result = append(result, r); return nil })
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].ProcessStartedAt.Equal(result[j].ProcessStartedAt) {
			return result[i].Sequence < result[j].Sequence
		}
		return result[i].ProcessStartedAt.Before(result[j].ProcessStartedAt)
	})
	return result
}

func writeRecord(t *testing.T, r *recordingFixture) {
	t.Helper()
	r.collect()
	if err := r.write(); err != nil {
		t.Fatal(err)
	}
}

func localHistory(r *recordingFixture) *history { return &r.sink.history }

func localRecorder(cfg config.Config, counters *Counters) *recordingFixture {
	sink := NewJSONLSink(cfg)
	return &recordingFixture{collector: newCollector(cfg, counters), sink: sink, delivery: delivery{sink: sink, interval: time.Duration(cfg.MetricsIntervalSeconds) * time.Second}}
}

func TestCollectorCapturesWithoutWritingHistory(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DaemonDir = t.TempDir()
	counters := &Counters{}
	c := newCollector(cfg, counters)
	c.sample = func(int) (procio.Sample, error) { return procio.Sample{DiskIO: procio.DiskIO{BytesWritten: 100}}, nil }
	counters.Flush(1, 0, 0, 0, 0, 0, 2*time.Second)
	c.collect()
	first := c.snapshot()
	counters.Flush(1, 0, 0, 0, 0, 0, time.Second)
	c.collect()
	second := c.snapshot()
	if first.Work.FlushMaxSeconds != 2 || second.Work.FlushMaxSeconds != 1 || second.Work.Inserted != 2 {
		t.Fatal("collector did not own independent sampling windows")
	}
	if first.Sequence != 1 || second.Sequence != 2 || first.IO.BytesWritten != 100 || first.IODelta != nil || first.WriteFailures != 0 {
		t.Fatal("collection contains delivery state")
	}
	if _, err := os.Stat(cfg.MetricsDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("collector wrote metrics history")
	}
}

func TestCountersConcurrentAndWindowPeaks(t *testing.T) {
	c := &Counters{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Event(true, 7)
				c.Event(false, 7)
				c.Flush(1, 2, 3, 4, 5, 6, time.Second)
				c.Preview(time.Millisecond, true)
				c.ScanStarted()
				c.ScanFailed()
				c.ScanFinished(10, time.Second)
			}
		}()
	}
	wg.Wait()
	w := c.take()
	if w.EventsReceived != 4000 || w.EventsAccepted != 2000 || w.EventsIgnored != 2000 || w.Inserted != 2000 || w.ScanActive != 0 || w.Scanned != 20000 || w.FlushSeconds != 2000 || w.PreviewFailures != 2000 {
		t.Fatalf("incorrect counters: %+v", w)
	}
	c.SetPending(0)
	next := c.take()
	if next.Flushes != w.Flushes || next.PendingMax != 7 || next.FlushMaxSeconds != 0 {
		t.Fatalf("incorrect reset: %+v", next)
	}
	var disabled *Counters
	disabled.Event(true, 1)
	disabled.Flush(1, 1, 1, 1, 1, 1, time.Second)
	disabled.Preview(time.Second, true)
	disabled.ScanStarted()
	disabled.ScanFailed()
	disabled.ScanFinished(1, time.Second)
	disabled.SetIndexed(1)
	disabled.SetPending(1)
	if disabled.take() != (Work{}) {
		t.Fatal("disabled counters must be empty")
	}
}

func TestRecorderDeltasAndSampledPeaks(t *testing.T) {
	r, now, sample := testRecorder(t)
	writeRecord(t, r)
	*now = now.Add(time.Minute)
	sample.ResidentBytes = 400
	sample.FootprintBytes = 500
	r.collect()
	sample.ResidentBytes = 40
	sample.FootprintBytes = 50
	sample.BytesRead += 10
	sample.BytesWritten += 20
	sample.LogicalBytesWritten += 15
	writeRecord(t, r)
	got := records(t, r.cfg.MetricsDir())
	if len(got) != 2 || got[0].IODelta.BytesWritten != 200 || got[1].IODelta.BytesWritten != 20 || got[1].IODelta.BytesRead != 10 || got[1].IODelta.LogicalBytesWritten != 15 {
		t.Fatalf("incorrect deltas: %+v", got)
	}
	if got[1].ProcessPeaks.ResidentMaxBytes != 400 || got[1].ProcessPeaks.FootprintMaxBytes != 500 || got[1].IntervalSeconds != 60 || got[1].Gap {
		t.Fatalf("incorrect peak/window: %+v", got[1])
	}
	if got[1].DatabaseBytes == nil || *got[1].DatabaseBytes != 20 || got[1].WALBytes != nil {
		t.Fatal("missing sizes must not become zero")
	}
	if !got[1].IOIntervalStartedAt.Equal(got[0].Timestamp) {
		t.Fatal("wrong I/O interval start")
	}
}

func TestWriteFailurePreservesDeltasAndPeaks(t *testing.T) {
	r, now, sample := testRecorder(t)
	writeRecord(t, r)
	originalDir := localHistory(r).dir
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	localHistory(r).dir = blocker
	*now = now.Add(time.Minute)
	sample.BytesWritten += 30
	r.counters.Flush(1, 0, 0, 0, 0, 0, 2*time.Second)
	r.collect()
	if r.write() == nil {
		t.Fatal("expected write failure")
	}
	localHistory(r).dir = originalDir
	*now = now.Add(time.Minute)
	sample.BytesWritten += 40
	writeRecord(t, r)
	got := records(t, originalDir)
	if len(got) != 2 || got[1].IODelta.BytesWritten != 70 || got[1].IntervalSeconds != 120 || !got[1].Gap || got[1].WriteFailures != 1 || got[1].Work.FlushMaxSeconds != 2 {
		t.Fatalf("failed write lost metrics: %+v", got)
	}
}

func TestFailedFinalSamplePreservesObservedPeaks(t *testing.T) {
	r, now, sample := testRecorder(t)
	sample.ResidentBytes, sample.FootprintBytes = 900, 1000
	r.collect()
	r.sample = func(int) (procio.Sample, error) { return procio.Sample{}, errors.New("unavailable") }
	writeRecord(t, r)
	*now = now.Add(time.Minute)
	writeRecord(t, r) // No valid samples in this window.
	got := records(t, r.cfg.MetricsDir())
	if got[0].Process != nil || got[0].IO != nil || !got[0].Gap || got[0].SampleFailures != 1 {
		t.Fatalf("failed sample fabricated current values: %+v", got[0])
	}
	if got[0].ProcessPeaks == nil || *got[0].ProcessPeaks != (ProcessPeaks{900, 1000}) {
		t.Fatalf("failed final sample lost known peaks: %+v", got[0].ProcessPeaks)
	}
	if got[1].ProcessPeaks != nil {
		t.Fatalf("empty window reused stale peaks: %+v", got[1].ProcessPeaks)
	}
}

func TestMissingSamplesAndCounterResets(t *testing.T) {
	r, now, sample := testRecorder(t)
	writeRecord(t, r)
	collector := r.sample
	r.sample = func(int) (procio.Sample, error) {
		return procio.Sample{}, errors.New("permission denied /Users/private/project-secret")
	}
	*now = now.Add(time.Minute)
	writeRecord(t, r)
	r.sample = collector
	*now = now.Add(time.Minute)
	sample.BytesWritten += 80
	writeRecord(t, r)
	*now = now.Add(time.Minute)
	sample.BytesWritten = 1
	writeRecord(t, r)
	*now = now.Add(time.Minute)
	sample.BytesWritten = 5
	writeRecord(t, r)
	got := records(t, r.cfg.MetricsDir())
	if got[1].IO != nil || got[1].Process != nil || !got[1].Gap || got[1].SampleFailures != 1 {
		t.Fatalf("unavailable sample fabricated data: %+v", got[1])
	}
	if got[2].IODelta.BytesWritten != 80 || !got[2].Gap || !got[2].IOIntervalStartedAt.Equal(got[0].Timestamp) {
		t.Fatalf("missing-sample baseline lost: %+v", got[2])
	}
	if !got[3].CounterReset || got[3].IODelta != nil || got[4].IODelta.BytesWritten != 4 {
		t.Fatal("reset must not underflow")
	}
}

func TestSleepAndBackwardClock(t *testing.T) {
	r, now, sample := testRecorder(t)
	writeRecord(t, r)
	*now = now.Add(12 * time.Hour)
	sample.BytesWritten += 10
	writeRecord(t, r)
	*now = now.Add(-time.Hour)
	sample.BytesWritten += 5
	writeRecord(t, r)
	got := records(t, r.cfg.MetricsDir())
	if !got[1].Gap || got[1].IntervalSeconds != 12*3600 || got[1].IODelta.BytesWritten != 10 {
		t.Fatal("sleep gap must be preserved")
	}
	if !got[2].ClockChanged || !got[2].Gap || got[2].IntervalSeconds != 0 {
		t.Fatal("backward clock must be flagged")
	}
}

func TestDailyTotalsAcrossRestartsAndUTC(t *testing.T) {
	r, now, sample := testRecorder(t)
	*now = time.Date(2026, 10, 29, 23, 59, 0, 0, time.UTC)
	r.started = now.Add(-time.Minute)
	writeRecord(t, r)
	*now = now.Add(time.Minute)
	sample.BytesWritten += 10
	writeRecord(t, r)
	// A different daemon lifetime starts at zero, without subtracting the old process.
	r2 := localRecorder(r.cfg, &Counters{})
	r2.started = now.Add(time.Minute)
	r2.now = r.now
	r2.sample = func(int) (procio.Sample, error) { return procio.Sample{DiskIO: procio.DiskIO{BytesWritten: 7}}, nil }
	*now = now.Add(2 * time.Minute)
	writeRecord(t, r2)
	totals := map[string]uint64{}
	for _, record := range records(t, r.cfg.MetricsDir()) {
		totals[record.Timestamp.Format("2006-01-02")] += record.IODelta.BytesWritten
	}
	if totals["2026-10-29"] != 200 || totals["2026-10-30"] != 17 {
		t.Fatalf("wrong end-of-interval daily totals: %v", totals)
	}
	files, err := localHistory(r).files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected UTC rotation, got %d files", len(files))
	}
}

func TestStrictPrivacyAndRecordBudget(t *testing.T) {
	r, _, sample := testRecorder(t)
	sample.BytesWritten = 535442284544
	sample.BytesRead = 30633111552
	sample.LogicalBytesWritten = 287707726274
	sample.ResidentBytes = 700 << 20
	sample.FootprintBytes = 900 << 20
	sample.CPUUserSeconds = 3600.123456789
	r.counters.Flush(1000, 2000, 300, 4000, 5000, 6, 123456789*time.Nanosecond)
	r.counters.Preview(987654321*time.Nanosecond, true)
	r.counters.ScanStarted()
	r.counters.ScanFinished(2661303, time.Hour)
	writeRecord(t, r)
	got := records(t, r.cfg.MetricsDir())
	data, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var check func(any, string)
	check = func(value any, key string) {
		switch value := value.(type) {
		case map[string]any:
			for k, v := range value {
				check(v, k)
			}
		case string:
			if key != "timestamp" && key != "process_started_at" && key != "io_interval_started_at" {
				t.Fatalf("unexpected text field %q", key)
			}
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				t.Fatal(err)
			}
		case float64, bool, nil:
		default:
			t.Fatalf("unexpected schema type: %T", value)
		}
	}
	check(fields, "")
	for _, secret := range []string{"sensitive-user", "project-secret", "private-document", "/Users/", "permission denied"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("privacy leak: %s", secret)
		}
	}
	if len(data) > 2304 {
		t.Fatalf("record budget exceeded: %d bytes", len(data))
	}
	t.Logf("representative record: %d bytes; 30/90/180 days: %.1f/%.1f/%.1f MiB", len(data)+1, float64(len(data)+1)*1440*30/(1<<20), float64(len(data)+1)*1440*90/(1<<20), float64(len(data)+1)*1440*180/(1<<20))
}

func TestDisabledAndWarnLevelCollection(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DaemonDir = filepath.Join(t.TempDir(), "daemon")
	stop := Start(cfg, nil, nil)
	stop()
	if _, err := os.Stat(cfg.MetricsDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled collection created files")
	}
	cfg.MetricsEnabled = true
	cfg.DaemonLogFile = filepath.Join(t.TempDir(), "daemon.log")
	cfg.DaemonLogLevel = "warn"
	logger, err := logging.OpenDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	stop = Start(cfg, &Counters{}, logger.Logger)
	stop()
	got := records(t, cfg.MetricsDir())
	if len(got) < 1 || got[len(got)-1].Sequence != 2 {
		t.Fatalf("expected final snapshot at warn level, got %+v", got)
	}
	for _, record := range got {
		if record.SchemaVersion != 1 {
			t.Fatal("invalid schema")
		}
	}
	files, err := os.ReadDir(cfg.MetricsDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(cfg.MetricsDir(), files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unsafe metrics mode %v", info.Mode())
	}
}

func TestRecorderPeriodicAndFinalSnapshots(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DaemonDir = t.TempDir()
	// Shorten the internal timer for this test; config.Load enforces >=15s.
	cfg.MetricsIntervalSeconds = 1
	counters := &Counters{}
	r := localRecorder(cfg, counters)
	r.sample = func(int) (procio.Sample, error) { return procio.Sample{}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx, func(record Record) {
			if err := r.delivery.write(context.Background(), record); err != nil {
				t.Error(err)
			}
		})
	}()
	defer func() { cancel(); <-done }()
	counters.Event(true, 1)
	deadline := time.Now().Add(5 * time.Second)
	for len(records(t, cfg.MetricsDir())) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	got := records(t, cfg.MetricsDir())
	if len(got) < 3 || got[len(got)-1].Work.EventsAccepted != 1 || got[len(got)-1].Sequence != uint64(len(got)) {
		t.Fatalf("periodic/final snapshots missing: %+v", got)
	}
}

func TestReadRecordsTruncatedAndInvalid(t *testing.T) {
	valid := "{\"schema_version\":1}\n"
	var count int
	if err := ReadRecords(strings.NewReader(valid+"{\"schema_version\":"), func(Record) error { count++; return nil }); err != nil || count != 1 {
		t.Fatalf("truncated read: %d %v", count, err)
	}
	for _, data := range []string{"broken\n", "{\"schema_version\":2}\n", strings.Repeat("x", 70<<10)} {
		if ReadRecords(strings.NewReader(data), func(Record) error { return nil }) == nil {
			t.Fatal("invalid input accepted")
		}
	}
	want := errors.New("stop")
	if err := ReadRecords(strings.NewReader(valid), func(Record) error { return want }); !errors.Is(err, want) {
		t.Fatal("visitor error lost")
	}
}

func TestNumericSchemaCannotGrowArbitraryContext(t *testing.T) {
	var inspect func(reflect.Type)
	inspect = func(typ reflect.Type) {
		if typ == reflect.TypeFor[time.Time]() {
			return
		}
		switch typ.Kind() {
		case reflect.Pointer:
			inspect(typ.Elem())
		case reflect.Struct:
			for i := range typ.NumField() {
				inspect(typ.Field(i).Type)
			}
		case reflect.Bool, reflect.Int, reflect.Int64, reflect.Uint32, reflect.Uint64, reflect.Float64:
		default:
			t.Fatalf("unbounded/context-carrying schema type: %v", typ)
		}
	}
	inspect(reflect.TypeFor[Record]())
}

func BenchmarkRecorderAppend(b *testing.B) {
	cfg, _ := config.Default()
	cfg.DaemonDir = b.TempDir()
	r := localRecorder(cfg, &Counters{})
	r.started = time.Now()
	r.sample = func(int) (procio.Sample, error) { return procio.Sample{DiskIO: procio.DiskIO{BytesWritten: 1000}}, nil }
	// Include scanning overhead after a full 180-day history exists.
	if err := os.MkdirAll(cfg.MetricsDir(), 0o700); err != nil {
		b.Fatal(err)
	}
	for age := 1; age < 180; age++ {
		name := "metrics-v1-" + time.Now().UTC().AddDate(0, 0, -age).Format("2006-01-02") + "-0000.jsonl"
		if err := os.WriteFile(filepath.Join(cfg.MetricsDir(), name), []byte("{}\n"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r.collect()
		if err := r.write(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRecorderMeasuredIOOverhead(t *testing.T) {
	r, now, _ := testRecorder(t)
	r.sample = procio.CurrentSample
	before, err := procio.CurrentSample(os.Getpid())
	if err != nil {
		t.Skip(err)
	}
	started := time.Now()
	for range 60 {
		*now = now.Add(time.Minute)
		writeRecord(t, r)
	}
	if err := localHistory(r).sync(); err != nil {
		t.Fatal(err)
	}
	wall := time.Since(started)
	after, err := procio.CurrentSample(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	files, err := localHistory(r).files()
	if err != nil {
		t.Fatal(err)
	}
	var payload int64
	for _, file := range files {
		payload += file.size
	}
	t.Logf("60 accelerated records: payload=%d bytes, elapsed=%s, process CPU=%.6fs, OS-attributed writes=%d bytes, logical writes=%d bytes", payload, wall,
		after.CPUUserSeconds+after.CPUSystemSeconds-before.CPUUserSeconds-before.CPUSystemSeconds,
		after.BytesWritten-before.BytesWritten, after.LogicalBytesWritten-before.LogicalBytesWritten)
	if payload > 60*2304 {
		t.Fatal("recording exceeded expected payload budget")
	}
}

func ExampleReadRecords() {
	input := strings.NewReader("{\"schema_version\":1,\"io_delta\":{\"written_bytes\":4096}}\n")
	var total uint64
	_ = ReadRecords(input, func(record Record) error {
		if record.IODelta != nil {
			total += record.IODelta.BytesWritten
		}
		return nil
	})
	fmt.Println(total)
	// Output: 4096
}
