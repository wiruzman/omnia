package metrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/procio"
)

type testSink struct {
	write func(context.Context, Record) error
	close func(context.Context) error
}

func (s testSink) Write(ctx context.Context, r Record) error { return s.write(ctx, r) }
func (s testSink) Close(ctx context.Context) error {
	if s.close != nil {
		return s.close(ctx)
	}
	return nil
}

func snapshot(sequence uint64) Record {
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	return Record{SchemaVersion: 1, Sequence: sequence, ProcessStartedAt: start, Timestamp: start.Add(time.Duration(sequence) * time.Minute),
		IO: &procio.DiskIO{BytesWritten: sequence * 10}, Process: &Process{CPUUserSeconds: float64(sequence)},
		ProcessPeaks: &ProcessPeaks{ResidentMaxBytes: sequence, FootprintMaxBytes: sequence * 2},
		Work:         Work{Inserted: sequence, FlushMaxSeconds: float64(sequence)}}
}

func receiveRecord(t *testing.T, ch <-chan Record) Record {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("sink did not receive snapshot")
		return Record{}
	}
}

func closeFanout(t *testing.T, f *fanout) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCoalescingPreservesPeaksAcrossUnavailableSamples(t *testing.T) {
	w := &sinkWorker{pending: make(chan Record, 1)}
	first := snapshot(1)
	first.ProcessPeaks = &ProcessPeaks{900, 1000}
	missing := snapshot(2)
	missing.Process, missing.IO, missing.ProcessPeaks = nil, nil, nil
	missing.Gap = true
	for _, r := range []Record{first, missing} {
		if err := w.publish(r); err != nil {
			t.Fatal(err)
		}
	}
	merged := <-w.pending
	if merged.Process != nil || merged.IO != nil || merged.ProcessPeaks == nil || *merged.ProcessPeaks != (ProcessPeaks{900, 1000}) {
		t.Fatalf("unavailable sample discarded peaks or fabricated current data: %+v", merged)
	}
	if err := w.publish(merged); err != nil {
		t.Fatal(err)
	}
	if err := w.publish(snapshot(3)); err != nil {
		t.Fatal(err)
	}
	merged = <-w.pending
	if merged.ProcessPeaks == nil || *merged.ProcessPeaks != (ProcessPeaks{900, 1000}) || merged.Process.CPUUserSeconds != 3 || !merged.Gap {
		t.Fatalf("recovered sample lost coalesced peaks: %+v", merged)
	}
}

func TestDeliveryFailurePreservesPeaksAcrossUnavailableSamples(t *testing.T) {
	var got Record
	d := delivery{interval: time.Minute, sink: testSink{write: func(_ context.Context, r Record) error {
		if r.Sequence < 3 {
			return errors.New("unavailable")
		}
		got = r
		return nil
	}}}
	first := snapshot(1)
	first.ProcessPeaks = &ProcessPeaks{900, 1000}
	missing := snapshot(2)
	missing.Process, missing.IO, missing.ProcessPeaks = nil, nil, nil
	for _, r := range []Record{first, missing} {
		if err := d.write(context.Background(), r); err == nil {
			t.Fatal("expected delivery failure")
		}
	}
	if err := d.write(context.Background(), snapshot(3)); err != nil {
		t.Fatal(err)
	}
	if got.ProcessPeaks == nil || *got.ProcessPeaks != (ProcessPeaks{900, 1000}) || got.WriteFailures != 2 || got.IODelta.BytesWritten != 30 {
		t.Fatalf("delivery recovery lost peaks/counters: %+v", got)
	}
}

func TestJSONLSinkCloseReportsSyncErrors(t *testing.T) {
	for _, failure := range []string{"directory missing", "file missing"} {
		t.Run(failure, func(t *testing.T) {
			cfg, err := config.Default()
			if err != nil {
				t.Fatal(err)
			}
			cfg.DaemonDir = t.TempDir()
			s := NewJSONLSink(cfg)
			if err := s.Close(context.Background()); err != nil {
				t.Fatalf("unused sink close: %v", err)
			}
			if err := s.Write(context.Background(), snapshot(1)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatalf("valid history sync: %v", err)
			}
			path := filepath.Join(s.history.dir, s.history.lastFile)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if failure == "directory missing" {
				if err := os.Remove(s.history.dir); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(context.Background()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("close did not propagate missing history: %v", err)
			}
		})
	}
}

func TestSlowSinkIsIsolatedAndQueueCoalescesWithoutLosingTotals(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fastRecords := make(chan Record, 32)
	var slowRecords []Record
	f := newFanout(time.Minute, nil,
		testSink{write: func(_ context.Context, r Record) error { fastRecords <- r; return nil }},
		testSink{write: func(ctx context.Context, r Record) error {
			if r.Sequence == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			slowRecords = append(slowRecords, r)
			return nil
		}})
	defer f.cancel()
	if err := f.Publish(snapshot(1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("slow worker not started")
	}
	fastTotal := receiveRecord(t, fastRecords).IODelta.BytesWritten
	for seq := uint64(2); seq <= 20; seq++ {
		r := snapshot(seq)
		if seq == 9 {
			r.ProcessPeaks.ResidentMaxBytes = 1000
			r.Work.FlushMaxSeconds = 1000
		}
		if err := f.Publish(r); err != nil {
			t.Fatal(err)
		}
		got := receiveRecord(t, fastRecords)
		if got.Sequence != seq || got.IODelta.BytesWritten != 10 {
			t.Fatalf("slow sink affected fast sink: %+v", got)
		}
		fastTotal += got.IODelta.BytesWritten
	}
	q := f.workers[1]
	q.mu.Lock()
	count, coalesced := len(q.pending), q.coalesced
	q.mu.Unlock()
	if count != 1 || coalesced != 18 {
		t.Fatalf("queue not bounded/coalesced: %d %d", count, coalesced)
	}
	close(release)
	closeFanout(t, f)
	var slowTotal uint64
	for _, r := range slowRecords {
		slowTotal += r.IODelta.BytesWritten
	}
	last := slowRecords[len(slowRecords)-1]
	if fastTotal != 200 || slowTotal != 200 || len(slowRecords) != 2 || last.Sequence != 20 || last.Work.Inserted != 20 || last.IODelta.BytesWritten != 190 {
		t.Fatalf("coalescing lost cumulative totals: fast=%d slow=%d last=%+v", fastTotal, slowTotal, last)
	}
	if !last.Gap || last.SnapshotsCoalesced != 18 || last.ProcessPeaks.ResidentMaxBytes != 1000 || last.Work.FlushMaxSeconds != 1000 {
		t.Fatalf("coalescing lost peaks or visibility: %+v", last)
	}
}

func TestFailuresAndBaselinesArePerSink(t *testing.T) {
	fast := make(chan Record, 4)
	recovered := make(chan Record, 4)
	attempted := make(chan struct{})
	f := newFanout(time.Minute, nil,
		testSink{write: func(_ context.Context, r Record) error { fast <- r; return nil }},
		testSink{write: func(_ context.Context, r Record) error {
			if r.Sequence == 1 {
				close(attempted)
				return errors.New("unavailable /private/context-must-not-be-exported")
			}
			recovered <- r
			return nil
		}})
	defer f.cancel()
	r := snapshot(1)
	r.Work.FlushMaxSeconds = 100
	if err := f.Publish(r); err != nil {
		t.Fatal(err)
	}
	receiveRecord(t, fast)
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("failed sink not attempted")
	}
	if err := f.Publish(snapshot(2)); err != nil {
		t.Fatal(err)
	}
	good, bad := receiveRecord(t, fast), receiveRecord(t, recovered)
	closeFanout(t, f)
	if good.WriteFailures != 0 || good.IODelta.BytesWritten != 10 || good.Work.FlushMaxSeconds != 2 {
		t.Fatalf("failure crossed sink boundary: %+v", good)
	}
	if bad.WriteFailures != 1 || bad.IODelta.BytesWritten != 20 || bad.Work.FlushMaxSeconds != 100 || !bad.Gap {
		t.Fatalf("failure lost baseline/peaks: %+v", bad)
	}
}

func TestAdapterMutationCannotCorruptOtherSinksOrBaselines(t *testing.T) {
	got := make(chan Record, 4)
	f := newFanout(time.Minute, nil,
		testSink{write: func(_ context.Context, r Record) error {
			r.IO.BytesWritten = 999999
			r.Process.CPUUserSeconds = 999999
			r.ProcessPeaks.ResidentMaxBytes = 999999
			return nil
		}},
		testSink{write: func(_ context.Context, r Record) error { got <- r; return nil }})
	defer f.cancel()
	first := snapshot(1)
	if err := f.Publish(first); err != nil {
		t.Fatal(err)
	}
	r1 := receiveRecord(t, got)
	if err := f.Publish(snapshot(2)); err != nil {
		t.Fatal(err)
	}
	r2 := receiveRecord(t, got)
	closeFanout(t, f)
	if first.IO.BytesWritten != 10 || first.Process.CPUUserSeconds != 1 || first.ProcessPeaks.ResidentMaxBytes != 1 || r1.ProcessPeaks.ResidentMaxBytes != 1 || r2.ProcessPeaks.ResidentMaxBytes != 2 || r1.IO.BytesWritten != 10 || r2.IODelta.BytesWritten != 10 || r2.CounterReset {
		t.Fatal("adapter mutated a shared snapshot/baseline")
	}
}

func TestShutdownDeadlineCancelsBlockedSink(t *testing.T) {
	entered := make(chan struct{})
	f := newFanout(time.Minute, nil, testSink{write: func(ctx context.Context, _ Record) error { close(entered); <-ctx.Done(); return ctx.Err() }})
	if err := f.Publish(snapshot(1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown did not respect deadline: %v", err)
	}
	select {
	case <-f.workers[0].done:
	case <-time.After(time.Second):
		t.Fatal("blocked worker leaked after cancellation")
	}
	if !errors.Is(f.Publish(snapshot(2)), errSinkClosed) {
		t.Fatal("write accepted after shutdown")
	}
}

func TestExtraSinkUsesSameSnapshotsAsLocalHistory(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DaemonDir = t.TempDir()
	var mu sync.Mutex
	var exported []Record
	sink := testSink{write: func(_ context.Context, r Record) error {
		mu.Lock()
		defer mu.Unlock()
		exported = append(exported, r)
		return nil
	}}
	stop := Start(cfg, &Counters{}, nil, sink)
	stop()
	if len(exported) != 0 {
		t.Fatal("disabled collection started exporter")
	}
	cfg.MetricsEnabled = true
	stop = Start(cfg, &Counters{}, nil, sink)
	stop()
	local := records(t, cfg.MetricsDir())
	if len(local) < 1 || len(exported) < 1 {
		t.Fatal("sink missed the final snapshot")
	}
	lastLocal, lastExported := local[len(local)-1], exported[len(exported)-1]
	if lastLocal.Sequence != 2 || lastExported.Sequence != 2 || lastLocal.ProcessStartedAt != lastExported.ProcessStartedAt || lastLocal.IO.BytesWritten != lastExported.IO.BytesWritten {
		t.Fatal("sinks do not share the source's final snapshot identity and totals")
	}
}
