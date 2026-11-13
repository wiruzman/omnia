package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/daemonstate"
	"github.com/wiruzman/omnia/internal/indexer"
	"github.com/wiruzman/omnia/internal/metrics"
	"github.com/wiruzman/omnia/internal/store"
)

func isolatedService(t *testing.T) (*Service, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.IncludePaths = []string{root}
	cfg.DaemonDir = filepath.Join(root, ".runtime")
	cfg.IndexDBPath = filepath.Join(cfg.DaemonDir, "index.sqlite")
	cfg.MetricsEnabled = true
	cfg.ScanThrottleMs = 0
	svc, err := New(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	// Seed storage, but not metric work counters, to suppress initial scans.
	seed := filepath.Join(root, "seed.txt")
	if err := os.WriteFile(seed, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	entry, err := svc.scanner.EntryFromPath(seed, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.UpsertEntryIfChanged(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	return svc, root
}

func testWatcher(svc *Service, capacity int) *fileWatcher {
	w := &fileWatcher{events: make(chan fileEvent, capacity), overflow: make(chan struct{}, 1), accept: svc.shouldTrackPathChange, metrics: svc.metrics, roots: svc.cfg.IncludePaths}
	svc.watcher = w
	return w
}

func runIsolated(t *testing.T, svc *Service) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	var stopped atomic.Bool
	stop := func() {
		if stopped.Swap(true) {
			return
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("isolated service shutdown timed out")
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitUntil(t *testing.T, timeout time.Duration, label string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("timeout: " + label)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIncrementalDebounceHasMaximumAge(t *testing.T) {
	first := time.Now()
	for _, tc := range []struct{ elapsed, delay time.Duration }{{0, 3 * time.Second}, {8 * time.Second, 2 * time.Second}, {10 * time.Second, 0}, {time.Hour, 0}} {
		if got := incrementalFlushDelay(first, first.Add(tc.elapsed)); got != tc.delay {
			t.Fatalf("elapsed=%s delay=%s want=%s", tc.elapsed, got, tc.delay)
		}
	}
}

func TestContinuousEventsFlushBeforeQuietPeriod(t *testing.T) {
	svc, root := isolatedService(t)
	w := testWatcher(svc, 64)
	path := filepath.Join(root, "continuous.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	runIsolated(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.publish(path)
			}
		}
	}()
	defer func() { cancel(); <-writerDone }()
	waitUntil(t, 13*time.Second, "flush during continuous event traffic", func() bool { return svc.metrics.Snapshot().Inserted > 0 })
	if svc.metrics.Snapshot().ScanRuns != 0 {
		t.Fatal("ordinary repeated writes caused a recovery scan")
	}
}

func TestFlushPendingBoundsWorkIncludingLargeDirectoryDeletion(t *testing.T) {
	svc, root := isolatedService(t)
	ctx := context.Background()
	pending := map[string]pendingWork{}
	directory := filepath.Join(root, "deleted-tree")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		path := filepath.Join(directory, fmt.Sprintf("file-%04d.txt", i))
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		pending[path] = 0
	}
	stats, err := svc.flushPending(ctx, pending)
	if err != nil || stats.Inserted != incrementalBatchSize || len(pending) != 300-incrementalBatchSize {
		t.Fatalf("unbounded batch: %+v pending=%d err=%v", stats, len(pending), err)
	}
	for len(pending) > 0 {
		if _, err := svc.flushPending(ctx, pending); err != nil {
			t.Fatal(err)
		}
	}
	// Directory deletion has one pending path but many database rows.
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	pending = map[string]pendingWork{directory: 0}
	stats, err = svc.flushPending(ctx, pending)
	if err != nil || stats.Deletes != incrementalBatchSize || len(pending) != 1 {
		t.Fatalf("unbounded prefix delete: %+v pending=%d err=%v", stats, len(pending), err)
	}
}

type failingChangeStore struct{ store.Backend }

func (s failingChangeStore) ApplyChanges(context.Context, []store.Change, int) ([]store.ChangeResult, error) {
	return nil, errors.New("fixture failure")
}

func TestFailedBatchRetainsPendingPaths(t *testing.T) {
	svc, root := isolatedService(t)
	path := filepath.Join(root, "retry.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	pending := map[string]pendingWork{path: 0}
	original := svc.store
	svc.store = failingChangeStore{original}
	if stats, err := svc.flushPending(context.Background(), pending); err == nil || stats.Inserted != 0 || len(pending) != 1 {
		t.Fatalf("lost failed batch: %+v %v", stats, err)
	}
	svc.store = original
	if stats, err := svc.flushPending(context.Background(), pending); err != nil || stats.Inserted != 1 || len(pending) != 0 {
		t.Fatalf("retry: %+v %v", stats, err)
	}
}

func TestWatcherOverflowReconcilesDroppedChanges(t *testing.T) {
	svc, root := isolatedService(t)
	w := testWatcher(svc, 2)
	path := filepath.Join(root, "otherwise-lost.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	w.publish(filepath.Join(root, "missing-1"))
	w.publish(filepath.Join(root, "missing-2"))
	w.publish(path)
	if len(w.events) != 2 || len(w.overflow) != 1 {
		t.Fatal("queue was not bounded or loss was silent")
	}
	runIsolated(t, svc)
	waitUntil(t, 8*time.Second, "overflow reconciliation completed", func() bool { m := svc.metrics.Snapshot(); return m.ScanRuns > 0 && m.ScanActive == 0 && m.Indexed >= 3 })
	if svc.metrics.Snapshot().WatcherOverflows != 1 {
		t.Fatal("overflow episode not counted")
	}
}

type failFirstScanStore struct {
	store.Backend
	attempts atomic.Int32
}

func (s *failFirstScanStore) BeginScan(ctx context.Context, id int64) error {
	if s.attempts.Add(1) == 1 {
		return errors.New("fixture recovery failure")
	}
	return s.Backend.BeginScan(ctx, id)
}

func TestFailedOverflowReconciliationRetries(t *testing.T) {
	svc, _ := isolatedService(t)
	w := testWatcher(svc, 2)
	backend := &failFirstScanStore{Backend: svc.store}
	svc.store = backend
	svc.indexer = indexer.NewWithMetrics(svc.cfg, svc.scanner, backend, svc.logger, svc.metrics)
	w.lost()
	runIsolated(t, svc)
	waitUntil(t, 8*time.Second, "failed reconciliation retried", func() bool {
		m := svc.metrics.Snapshot()
		return m.ScanRuns >= 2 && m.ScanActive == 0 && m.ScanFailures == 1 && m.Indexed >= 2
	})
}

func TestWatcherDoesNotRetainPathHistory(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	counters := &metrics.Counters{}
	w := &fileWatcher{events: make(chan fileEvent, 8), overflow: make(chan struct{}, 1), accept: func(path string) bool { return !filepath.IsAbs(path) }, metrics: counters}
	for i := 0; i < 200000; i++ {
		w.publish(fmt.Sprintf("/ignored/file-%08d", i))
	}
	if len(w.events) != 0 || len(w.overflow) != 0 || counters.Snapshot().EventsIgnored != 200000 {
		t.Fatal("ignored paths entered delivery or recovery")
	}
	w.accept = func(string) bool { return true }
	for i := 0; i < 200000; i++ {
		w.publish(fmt.Sprintf("file-%08d", i))
	}
	if len(w.events) != 8 || len(w.overflow) != 1 {
		t.Fatal("delivery storage grew beyond capacity")
	}
	for len(w.events) > 0 {
		<-w.events
	}
	runtime.GC()
	if len(w.events) != 0 {
		t.Fatal("drained paths retained")
	}
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("retained heap after 400,000 synthetic notifications and drain: %.2f MiB", float64(retained)/1048576)
	if retained > 8<<20 {
		t.Fatalf("path history retained: %d bytes", retained)
	}
	runtime.KeepAlive(w)
}

func TestPendingCapacityOverflowRequestsRecovery(t *testing.T) {
	svc, root := isolatedService(t)
	w := testWatcher(svc, pendingCapacity+2)
	for i := 0; i < pendingCapacity+1; i++ {
		w.publish(filepath.Join(root, fmt.Sprintf("missing-%06d", i)))
	}
	runIsolated(t, svc)
	waitUntil(t, 8*time.Second, "pending overflow recovered", func() bool {
		m := svc.metrics.Snapshot()
		return m.WatcherOverflows > 0 && m.ScanRuns > 0 && m.ScanActive == 0 && m.Indexed >= 2
	})
	if m := svc.metrics.Snapshot(); m.PendingMax > pendingCapacity {
		t.Fatalf("unbounded pending map: %+v", m)
	}
	if m := svc.metrics.Snapshot(); m.PendingOverflows != 1 || m.GlobalRecoveryScans != 1 {
		t.Fatalf("pending-capacity reason not recorded: %+v", m)
	}
}

func TestQueuedLossOnShutdownPersistsReconciliation(t *testing.T) {
	svc, root := isolatedService(t)
	w := testWatcher(svc, 1)
	w.publish(filepath.Join(root, "queued.txt"))
	w.lost()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc.cfg.DaemonTriggerPath()); err != nil {
		t.Fatalf("queued work was not persisted for recovery: %v", err)
	}
	triggered, err := daemonstate.ConsumeTrigger(svc.cfg.DaemonTriggerPath())
	if err != nil || !triggered {
		t.Fatalf("recovery trigger=%t error=%v", triggered, err)
	}
	if _, err := os.Stat(svc.cfg.DaemonFreshStartPath()); err != nil {
		t.Fatalf("lost events could resume past changed paths on restart: %v", err)
	}
}

func TestOverflowReconciliationDoesNotResumePastLostPaths(t *testing.T) {
	svc, root := isolatedService(t)
	w := testWatcher(svc, 2)
	path := filepath.Join(root, "a-lost.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := daemonstate.WriteResumeState(svc.cfg.DaemonResumeStatePath(), daemonstate.ReindexResumeState{ScanID: 1, Root: root, CurrentPath: filepath.Join(root, "zzzz"), Committed: true}); err != nil {
		t.Fatal(err)
	}
	w.lost()
	runIsolated(t, svc)
	waitUntil(t, 8*time.Second, "fresh overflow scan included paths before old resume cursor", func() bool { m := svc.metrics.Snapshot(); return m.ScanRuns > 0 && m.ScanActive == 0 && m.Indexed >= 3 })
}
