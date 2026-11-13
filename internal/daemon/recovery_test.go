//go:build darwin && cgo

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/indexer"
	"github.com/wiruzman/omnia/internal/metrics"
	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/store"
)

func cacheRecoveryFile(t *testing.T, svc *Service, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := svc.scanner.EntryFromPath(path, rootForPath(svc.cfg.IncludePaths, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.UpsertEntryIfChanged(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func recoveryHas(t *testing.T, svc *Service, query string) bool {
	t.Helper()
	r, err := svc.store.Query(context.Background(), query, sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(r.Entries) > 0
}

func drainRecovery(t *testing.T, svc *Service, pending map[string]pendingWork) {
	t.Helper()
	for turns := 0; len(pending) > 0; turns++ {
		if turns > 50 {
			t.Fatal("scoped work did not drain")
		}
		stats, err := svc.flushPending(context.Background(), pending)
		if err != nil || stats.Overflow {
			t.Fatalf("scoped drain: %+v %v", stats, err)
		}
	}
}

func TestNativeScopedRecoveryFindsChangesAndMissedDeletes(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag uint32
	}{{"subtree", 1}, {"root_changed", 0x20}, {"mount", 0x40}, {"unmount", 0x80}} {
		t.Run(tc.name, func(t *testing.T) {
			svc, root := isolatedService(t)
			scope := filepath.Join(root, "scope")
			stale := filepath.Join(scope, "staleneedle")
			outside := filepath.Join(root, "other", "outsideneedle")
			adjacent := filepath.Join(root, "scope-sibling", "adjacentneedle")
			for _, p := range []string{stale, outside, adjacent} {
				cacheRecoveryFile(t, svc, p)
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(scope, "newneedle"), []byte("new"), 0600); err != nil {
				t.Fatal(err)
			}
			w := testWatcher(svc, 16)
			w.nativeEvent(scope, tc.flag)
			stop := runIsolated(t, svc)
			waitUntil(t, 7*time.Second, "scoped changes drained", func() bool { m := svc.metrics.Snapshot(); return m.Deleted == 1 && m.Inserted >= 2 && m.Pending == 0 })
			stop()
			if !recoveryHas(t, svc, "newneedle") || recoveryHas(t, svc, "staleneedle") || !recoveryHas(t, svc, "outsideneedle") || !recoveryHas(t, svc, "adjacentneedle") {
				t.Fatal("scoped recovery missed work or changed siblings")
			}
			m := svc.metrics.Snapshot()
			if m.ScanRuns != 0 || m.WatcherOverflows != 0 || m.ScopedRecoveryRequests != 1 || m.GlobalRecoveryRequests != 0 {
				t.Fatalf("scoped event became global: %+v", m)
			}
		})
	}
}

func TestScopedRecoveryPreservesInaccessibleDescendants(t *testing.T) {
	svc, root := isolatedService(t)
	scope := filepath.Join(root, "scope")
	locked := filepath.Join(scope, "locked")
	cacheRecoveryFile(t, svc, filepath.Join(locked, "preserveneedle"))
	stale := filepath.Join(scope, "staleneedle")
	cacheRecoveryFile(t, svc, stale)
	if err := os.Remove(stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0700) })
	if _, err := os.ReadDir(locked); !os.IsPermission(err) {
		t.Skip("platform can enumerate inaccessible fixture")
	}
	w := testWatcher(svc, 8)
	w.nativeEvent(scope, 1)
	stop := runIsolated(t, svc)
	waitUntil(t, 7*time.Second, "permission-safe scope drained", func() bool { m := svc.metrics.Snapshot(); return m.Deleted == 1 && m.Pending == 0 })
	stop()
	if !recoveryHas(t, svc, "preserveneedle") || recoveryHas(t, svc, "staleneedle") {
		t.Fatal("inaccessible entry lost or missing entry retained")
	}
	if m := svc.metrics.Snapshot(); m.ScanRuns != 0 || m.Failures != 0 {
		t.Fatalf("unexpected global recovery or read failure: %+v", m)
	}
	// A second event targeted at the inaccessible directory preserves it too.
	drainRecovery(t, svc, map[string]pendingWork{locked: discoverChildren})
	if !recoveryHas(t, svc, "preserveneedle") {
		t.Fatal("direct inaccessible scope lost entries")
	}
	// Cached-descendant inspection must honor a subsequent permission failure.
	drainRecovery(t, svc, map[string]pendingWork{filepath.Join(locked, "preserveneedle"): checkAncestors})
	if !recoveryHas(t, svc, "preserveneedle") {
		t.Fatal("cached-descendant inspection bypassed access protection")
	}
}

func TestScopedRecoveryCleansReplacedDirectoryWithoutIndexingSymlinkTarget(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		name := "file"
		if symlink {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			svc, root := isolatedService(t)
			scope := filepath.Join(root, "scope")
			old := filepath.Join(scope, "oldneedle")
			cacheRecoveryFile(t, svc, old)
			if err := os.Remove(old); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(scope); err != nil {
				t.Fatal(err)
			}
			if symlink {
				target := t.TempDir()
				if err := os.WriteFile(filepath.Join(target, "oldneedle"), []byte("target"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, scope); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(scope, []byte("file"), 0600); err != nil {
				t.Fatal(err)
			}
			drainRecovery(t, svc, map[string]pendingWork{scope: discoverChildren})
			if recoveryHas(t, svc, "oldneedle") {
				t.Fatal("obsolete descendants retained or symlink followed")
			}
		})
	}
}

func TestScopedRecoveryCleansDirectoryReplacedBySymlinkLoop(t *testing.T) {
	svc, root := isolatedService(t)
	scope := filepath.Join(root, "scope")
	child := filepath.Join(scope, "oldneedle")
	cacheRecoveryFile(t, svc, child)
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(scope); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(scope, scope); err != nil {
		t.Fatal(err)
	}
	drainRecovery(t, svc, map[string]pendingWork{scope: discoverChildren})
	if recoveryHas(t, svc, "oldneedle") {
		t.Fatal("unreachable descendants of symlink loop retained")
	}
}

func TestScopedRecoveryBudgetFallsBackToGlobal(t *testing.T) {
	svc, root := isolatedService(t)
	scope := filepath.Join(root, "scope")
	if err := os.Mkdir(scope, 0700); err != nil {
		t.Fatal(err)
	}
	entries := make([]model.Entry, pendingCapacity+1)
	for i := range entries {
		path := filepath.Join(scope, time.Unix(int64(i), 0).Format("150405"))
		entries[i] = model.Entry{Path: path, Name: filepath.Base(path), ParentPath: scope, RootPath: root, Type: model.TypeFile}
	}
	if err := svc.store.UpsertBatch(context.Background(), 1, entries); err != nil {
		t.Fatal(err)
	}
	w := testWatcher(svc, 8)
	w.nativeEvent(scope, 1)
	stop := runIsolated(t, svc)
	waitUntil(t, 12*time.Second, "oversize cached scope recovered", func() bool {
		m := svc.metrics.Snapshot()
		return m.ScanRuns > 0 && m.ScanActive == 0 && m.DiscoveryOverflows > 0 && m.Indexed == 3
	})
	stop()
	m := svc.metrics.Snapshot()
	if m.DiscoveryOverflows != 1 || m.GlobalRecoveryRequests != 1 || m.GlobalRecoveryScans != 1 || m.PendingMax > pendingCapacity {
		t.Fatalf("unbounded or misclassified fallback: %+v", m)
	}
}

func TestRecoveryScopeClipsAncestorsAndHonorsIgnoredPaths(t *testing.T) {
	for _, roots := range [][]string{{"/watch/a", "/watch/b"}, {"/watch", "/watch/nested"}, {"/"}} {
		w := &fileWatcher{events: make(chan fileEvent, 8), overflow: make(chan struct{}, 1), metrics: &metrics.Counters{}, roots: roots, accept: func(p string) bool { return rootForPath(roots, p) != "" && p != "/watch/ignored" }}
		w.nativeEvent("/watch", 1)
		want := 1
		if len(roots) > 0 && roots[0] == "/watch/a" {
			want = 2
		}
		if len(w.events) != want || len(w.overflow) != 0 {
			t.Fatalf("roots %v events=%d overflow=%d", roots, len(w.events), len(w.overflow))
		}
		for len(w.events) > 0 {
			e := <-w.events
			if !w.accept(e.path) || !e.discover {
				t.Fatalf("invalid scope: %+v", e)
			}
		}
		w.nativeEvent("/watch/ignored", 0x40)
		if len(w.events) != 0 || len(w.overflow) != 0 {
			t.Fatal("ignored mount triggered work")
		}
		w.nativeEvent("/watch/ignored", 1|2|4|8)
		if len(w.overflow) != 1 || len(w.events) != 0 {
			t.Fatal("true loss was filtered by path")
		}
		m := w.metrics.Snapshot()
		if m.NativeMustScan != 2 || m.NativeUserDropped != 1 || m.NativeKernelDropped != 1 || m.NativeIDsWrapped != 1 || m.NativeMount != 1 || m.WatcherOverflows != 1 {
			t.Fatalf("incorrect native reason counts: %+v", m)
		}
	}
}

func TestDeliveryOverflowCountsRawDropsSeparatelyFromCoalescing(t *testing.T) {
	w := &fileWatcher{events: make(chan fileEvent, 1), overflow: make(chan struct{}, 1), accept: func(string) bool { return true }, metrics: &metrics.Counters{}}
	w.publish("/one")
	w.publish("/two")
	w.publish("/three")
	m := w.metrics.Snapshot()
	if m.DeliveryOverflows != 2 || m.WatcherOverflows != 1 || m.EventsAccepted != 3 {
		t.Fatalf("drop/coalescing accounting: %+v", m)
	}
}

func TestScopedRecoveryKeepsAncestryCheckDiscoveredLaterInBatch(t *testing.T) {
	svc, root := isolatedService(t)
	scope := filepath.Join(root, "scope")
	child := filepath.Join(scope, "oldneedle")
	cacheRecoveryFile(t, svc, child)
	old, err := svc.scanner.EntryFromPath(child, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(scope); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "oldneedle"), []byte("untracked target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, scope); err != nil {
		t.Fatal(err)
	}
	// Exercise both Go map iteration orders: a child may already be queued for
	// upsert when its parent requests reinspection of cached descendants.
	for range 64 {
		if _, err := svc.store.UpsertEntryIfChanged(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		drainRecovery(t, svc, map[string]pendingWork{scope: discoverChildren, child: 0})
		if recoveryHas(t, svc, "oldneedle") {
			t.Fatal("committed child upsert discarded later ancestry check")
		}
	}
}

type recoveryGateStore struct {
	store.Backend
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (s *recoveryGateStore) BeginScan(ctx context.Context, id int64) error {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Backend.BeginScan(ctx, id)
}

func TestTrueNativeLossDuringRecoveryKeepsGlobalFollowup(t *testing.T) {
	svc, root := isolatedService(t)
	stale := filepath.Join(root, "outside-scope", "staleneedle")
	cacheRecoveryFile(t, svc, stale)
	if err := os.Remove(stale); err != nil {
		t.Fatal(err)
	}
	w := testWatcher(svc, 8)
	gate := &recoveryGateStore{Backend: svc.store, entered: make(chan struct{}), release: make(chan struct{})}
	svc.store = gate
	svc.indexer = indexer.NewWithMetrics(svc.cfg, svc.scanner, gate, svc.logger, svc.metrics)
	// A loss flag is global even when its path would normally be ignored.
	w.nativeEvent("/untracked", 1|2)
	stop := runIsolated(t, svc)
	select {
	case <-gate.entered:
	case <-time.After(6 * time.Second):
		t.Fatal("global recovery did not start")
	}
	w.nativeEvent("/untracked", 1|4)
	waitUntil(t, 3*time.Second, "loss during scan consumed", func() bool { return len(w.overflow) == 0 })
	close(gate.release)
	waitUntil(t, 8*time.Second, "global follow-up complete", func() bool { m := svc.metrics.Snapshot(); return m.ScanRuns == 2 && m.ScanActive == 0 })
	stop()
	m := svc.metrics.Snapshot()
	if m.NativeUserDropped != 1 || m.NativeKernelDropped != 1 || m.NativeMustScan != 2 || m.GlobalRecoveryRequests != 2 || m.RecoveryDuringScan != 1 || m.GlobalRecoveryScans != 2 || m.ScanFailures != 0 || m.ScopedRecoveryRequests != 0 {
		t.Fatalf("global loss/follow-up accounting: %+v", m)
	}
	if recoveryHas(t, svc, "staleneedle") {
		t.Fatal("true loss did not clean outside reported path")
	}
}
