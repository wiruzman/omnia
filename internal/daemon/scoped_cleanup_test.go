//go:build darwin && cgo

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAncestorScopeDoesNotIndexReplacementSymlinkTarget(t *testing.T) {
	svc, root := isolatedService(t)
	scope := filepath.Join(root, "scope")
	replaced := filepath.Join(scope, "replaced")
	child := filepath.Join(replaced, "oldneedle")
	cacheRecoveryFile(t, svc, child)
	dir, err := svc.scanner.EntryFromPath(replaced, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.UpsertEntryIfChanged(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(replaced); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	for _, name := range []string{"oldneedle", "targetonlyneedle"} {
		if err := os.WriteFile(filepath.Join(target, name), []byte("outside target"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, replaced); err != nil {
		t.Fatal(err)
	}
	w := testWatcher(svc, 8)
	w.nativeEvent(scope, 1)
	stop := runIsolated(t, svc)
	waitUntil(t, 7*time.Second, "ancestor scoped recovery drained", func() bool { m := svc.metrics.Snapshot(); return m.Flushes > 0 && m.Pending == 0 })
	stop()
	if m := svc.metrics.Snapshot(); m.ScanRuns != 0 || m.Failures != 0 {
		t.Fatalf("unexpected global scan or failure: %+v", m)
	}
	if recoveryHas(t, svc, "oldneedle") || recoveryHas(t, svc, "targetonlyneedle") {
		t.Fatal("ancestor scope retained/indexed descendants behind a replacement symlink")
	}
}

func TestQueuedScopedWorkPreservesRestoredDirectories(t *testing.T) {
	for _, access := range []string{"readable", "inaccessible_child", "inaccessible_scope"} {
		t.Run(access, func(t *testing.T) {
			svc, root := isolatedService(t)
			scope := filepath.Join(root, "scope")
			nested := filepath.Join(scope, "nested")
			child := filepath.Join(nested, "preserveneedle")
			cacheRecoveryFile(t, svc, child)
			dir, err := svc.scanner.EntryFromPath(nested, root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.store.UpsertEntryIfChanged(context.Background(), dir); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{child, nested, scope} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(t.TempDir(), scope); err != nil {
				t.Fatal(err)
			}
			pending := map[string]pendingWork{}
			if overflow, err := svc.queueDirectoryChildren(context.Background(), scope, pending); err != nil || overflow {
				t.Fatalf("queue: overflow=%t err=%v", overflow, err)
			}
			if len(pending) != 2 {
				t.Fatalf("missing cached descendant work: %v", pending)
			}
			// The filesystem restores the directory before the queued work drains.
			if err := os.Remove(scope); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(nested, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(child, []byte("restored"), 0600); err != nil {
				t.Fatal(err)
			}
			if access != "readable" {
				locked := nested
				if access == "inaccessible_scope" {
					locked = scope
				}
				if err := os.Chmod(locked, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(locked, 0700) })
				if _, err := os.ReadDir(locked); !os.IsPermission(err) {
					t.Skip("platform can enumerate inaccessible fixture")
				}
			}
			drainRecovery(t, svc, pending)
			if !recoveryHas(t, svc, "preserveneedle") {
				t.Fatal("queued cleanup deleted a restored directory's cached descendant")
			}
			if m := svc.metrics.Snapshot(); m.Deleted != 0 || m.Failures != 0 {
				t.Fatalf("restored directories caused destructive cleanup or read failures: %+v", m)
			}
		})
	}
}
