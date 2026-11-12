package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/daemonstate"
	"github.com/wiruzman/omnia/internal/indexer"
	"github.com/wiruzman/omnia/internal/sorter"
)

func TestStaleChildDoesNotPoisonValidChanges(t *testing.T) {
	svc, root := isolatedService(t)
	parent := filepath.Join(root, "replaced-directory")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(parent, "child.txt")
	if err := os.WriteFile(child, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	entry, err := svc.scanner.EntryFromPath(child, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.UpsertEntryIfChanged(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("now a file"), 0600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "good.txt")
	if err := os.WriteFile(good, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{child: false, good: false}
	stats, err := svc.flushPending(context.Background(), pending)
	if err != nil || stats.Inserted != 1 || stats.Deletes != 1 || len(pending) != 0 {
		t.Fatalf("stale child blocked batch: %+v pending=%d err=%v", stats, len(pending), err)
	}
}

func TestPathReadFailureDoesNotAbortOtherChanges(t *testing.T) {
	svc, root := isolatedService(t)
	loop := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(loop, "child.txt")
	good := filepath.Join(root, "good.txt")
	if err := os.WriteFile(good, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{bad: false, good: false}
	stats, err := svc.flushPending(context.Background(), pending)
	if err == nil || stats.Inserted != 1 || stats.Failures != 1 || len(pending) != 1 {
		t.Fatalf("read failure poisoned batch: %+v pending=%d err=%v", stats, len(pending), err)
	}
	if _, exists := pending[bad]; !exists {
		t.Fatal("retryable path was lost")
	}
	if m := svc.metrics.Snapshot(); m.Failures != 1 || m.Inserted != 1 {
		t.Fatalf("incorrect failure accounting: %+v", m)
	}
	if err := os.Remove(loop); err != nil {
		t.Fatal(err)
	}
	if stats, err := svc.flushPending(context.Background(), pending); err != nil || len(pending) != 0 {
		t.Fatalf("retry did not finish: %+v %v", stats, err)
	}
}

func TestCommittedChangesUpdateStatusDespitePathReadFailure(t *testing.T) {
	svc, root := isolatedService(t)
	w := testWatcher(svc, 8)
	if err := daemonstate.Write(svc.cfg.DaemonStatusPath(), daemonstate.Status{IndexedTotal: 1}); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "good.txt")
	if err := os.WriteFile(good, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	w.publish(filepath.Join(loop, "child.txt"))
	w.publish(good)
	runIsolated(t, svc)
	waitUntil(t, 7*time.Second, "valid change committed beside read failure", func() bool { m := svc.metrics.Snapshot(); return m.Inserted == 1 && m.Failures > 0 && m.Indexed == 2 })
}

func TestFreshRecoveryConsumesRedundantTrigger(t *testing.T) {
	svc, root := isolatedService(t)
	w := testWatcher(svc, 2)
	w.publish(filepath.Join(root, "queued.txt"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Run(ctx); err != nil {
		t.Fatal(err)
	}
	testWatcher(svc, 2)
	stop := runIsolated(t, svc)
	waitUntil(t, 6*time.Second, "fresh recovery completed", func() bool { m := svc.metrics.Snapshot(); return m.ScanRuns > 0 && m.ScanActive == 0 })
	if _, err := os.Stat(svc.cfg.DaemonTriggerPath()); !os.IsNotExist(err) {
		t.Fatalf("redundant trigger remains: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if m := svc.metrics.Snapshot(); m.ScanRuns != 1 {
		t.Fatalf("recovery started %d scans", m.ScanRuns)
	}
	stop()
}

func TestFreshRequestCoalescesQueuedOverflow(t *testing.T) {
	svc, _ := isolatedService(t)
	w := testWatcher(svc, 2)
	if err := daemonstate.RequestFreshReindex(svc.cfg.DaemonFreshStartPath()); err != nil {
		t.Fatal(err)
	}
	if err := daemonstate.TriggerReindex(svc.cfg.DaemonTriggerPath()); err != nil {
		t.Fatal(err)
	}
	w.lost()
	runIsolated(t, svc)
	waitUntil(t, 6*time.Second, "coalesced fresh recovery", func() bool { m := svc.metrics.Snapshot(); return m.ScanRuns > 0 && m.ScanActive == 0 })
	time.Sleep(1200 * time.Millisecond)
	if m := svc.metrics.Snapshot(); m.ScanRuns != 1 {
		t.Fatalf("fresh and overflow requests started %d scans", m.ScanRuns)
	}
}

func TestFreshRecoveryRetriesAfterRedundantTriggerIsConsumed(t *testing.T) {
	svc, _ := isolatedService(t)
	testWatcher(svc, 2)
	backend := &failFirstScanStore{Backend: svc.store}
	svc.store = backend
	svc.indexer = indexer.NewWithMetrics(svc.cfg, svc.scanner, backend, svc.logger, svc.metrics)
	if err := daemonstate.RequestFreshReindex(svc.cfg.DaemonFreshStartPath()); err != nil {
		t.Fatal(err)
	}
	if err := daemonstate.TriggerReindex(svc.cfg.DaemonTriggerPath()); err != nil {
		t.Fatal(err)
	}
	runIsolated(t, svc)
	waitUntil(t, 8*time.Second, "failed fresh recovery retried without duplicate trigger", func() bool {
		m := svc.metrics.Snapshot()
		return m.ScanRuns == 2 && m.ScanActive == 0 && m.ScanFailures == 1 && m.Indexed >= 2
	})
	if _, err := os.Stat(svc.cfg.DaemonTriggerPath()); !os.IsNotExist(err) {
		t.Fatalf("redundant trigger remains: %v", err)
	}
}

func assertScopedEntries(t *testing.T, svc *Service, root string, want int) {
	t.Helper()
	r, err := svc.store.Query(context.Background(), "", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 100, 0)
	if err != nil || len(r.Entries) != want {
		t.Fatalf("entries=%d want=%d err=%v", len(r.Entries), want, err)
	}
	for _, e := range r.Entries {
		if e.RootPath != root || strings.Contains(e.Path, ".git") {
			t.Fatalf("incorrect discovery scope: %+v", e)
		}
	}
}

func TestDirectoryDiscoveryRespectsPendingCapacity(t *testing.T) {
	svc, root := isolatedService(t)
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "child.txt"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{directory: true}
	for i := 1; i < pendingCapacity; i++ {
		pending[filepath.Join(root, fmt.Sprintf("queued-%06d", i))] = false
	}
	overflow, err := svc.queueDirectoryChildren(context.Background(), directory, pending)
	if err != nil || !overflow || len(pending) != pendingCapacity {
		t.Fatalf("discovery exceeded bound: overflow=%t pending=%d err=%v", overflow, len(pending), err)
	}
}

func TestDirectoryDiscoverySurvivesBatchRollback(t *testing.T) {
	svc, root := isolatedService(t)
	directory := filepath.Join(root, "directory")
	if err := os.MkdirAll(filepath.Join(directory, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "nested", "child.txt"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{directory: true}
	original := svc.store
	svc.store = failingChangeStore{original}
	if _, err := svc.flushPending(context.Background(), pending); err == nil || len(pending) == 0 {
		t.Fatal("failed batch discarded discovery work")
	}
	svc.store = original
	for turns := 0; len(pending) > 0; turns++ {
		if turns > 10 {
			t.Fatal("discovery retry did not finish")
		}
		if _, err := svc.flushPending(context.Background(), pending); err != nil {
			t.Fatal(err)
		}
	}
	assertScopedEntries(t, svc, root, 4)
}
