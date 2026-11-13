//go:build darwin && cgo

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/metrics"
)

func TestNativeWatcherReportsLossAndDirectoryMoves(t *testing.T) {
	w := &fileWatcher{events: make(chan fileEvent, 8), overflow: make(chan struct{}, 1), accept: func(path string) bool { return path != "/ignored" }, metrics: &metrics.Counters{}}
	// Actual stream loss remains global, even for an ignored event path.
	for _, flag := range []uint32{2, 4, 8} {
		w.nativeEvent("/fixture", flag)
		select {
		case <-w.overflow:
		default:
			t.Fatalf("lost native recovery flag %#x", flag)
		}
	}
	for _, flag := range []uint32{1, 0x20, 0x40, 0x80} {
		w.nativeEvent("/fixture", flag)
		if len(w.overflow) != 0 {
			t.Fatalf("scoped flag %#x requested global recovery", flag)
		}
		if e := <-w.events; e.path != "/fixture" || !e.discover || !e.scoped {
			t.Fatalf("missing scoped recovery: %+v", e)
		}
	}
	// ItemIsDir|ItemRenamed: descendants may have no individual events.
	w.nativeEvent("/fixture", 0x20000|0x800)
	if len(w.overflow) != 0 {
		t.Fatal("directory move requested a global scan")
	}
	if event := <-w.events; !event.discover || event.path != "/fixture" {
		t.Fatalf("missing scoped directory discovery: %+v", event)
	}
	w.nativeEvent("/ignored", 0x20000|0x100)
	if len(w.overflow) != 0 || len(w.events) != 0 {
		t.Fatal("ignored directory triggered indexing/recovery")
	}
	w.nativeEvent("/fixture", 0x20000|0x1000) // Ordinary directory metadata/write.
	if event := <-w.events; event.discover {
		t.Fatal("ordinary directory write requested descendant discovery")
	}
	if len(w.overflow) != 0 {
		t.Fatal("ordinary directory write requested global recovery")
	}
}

func TestNativeWatcherCreateModifyRenameDeleteAndClose(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := newFileWatcher([]string{root}, func(string) bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	expect := func(path string) {
		t.Helper()
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		for {
			select {
			case got := <-w.events:
				if got.path == path {
					return
				}
			case <-timer.C:
				t.Fatalf("no native event for %s", filepath.Base(path))
			}
		}
	}
	path := filepath.Join(root, "created.txt")
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	expect(path)
	// Drain coalesced create/inode notifications before checking modification.
	for len(w.events) > 0 {
		<-w.events
	}
	if err := os.WriteFile(path, []byte("second write"), 0600); err != nil {
		t.Fatal(err)
	}
	expect(path)
	renamed := filepath.Join(root, "renamed.txt")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	expect(renamed)
	for len(w.events) > 0 {
		<-w.events
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	expect(renamed)
	w.Close()
	w.Close() // Native handle release is idempotent.
}

func TestNativeEventStormReconcilesWithinBounds(t *testing.T) {
	svc, root := isolatedService(t)
	w, err := newFileWatcher([]string{root}, svc.shouldTrackPathChange, svc.metrics)
	if err != nil {
		t.Fatal(err)
	}
	svc.watcher = w
	t.Cleanup(w.Close)
	const files = pendingCapacity + 500
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("storm-%06d.txt", i)), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Pause the consumer until the native channel overflows; this does not
	// depend on disk speed or scheduling to reproduce a slow-reader episode.
	waitUntil(t, 8*time.Second, "native overflow signaled", func() bool { return len(w.overflow) > 0 })
	runIsolated(t, svc)
	waitUntil(t, 30*time.Second, "native storm reconciliation", func() bool {
		m := svc.metrics.Snapshot()
		return m.WatcherOverflows > 0 && m.ScanRuns > 0 && m.ScanActive == 0 && m.Indexed == files+2
	})
	if m := svc.metrics.Snapshot(); m.PendingMax > pendingCapacity {
		t.Fatalf("pending exceeded cap: %+v", m)
	}
}

func TestOrdinaryMkdirDoesNotStartGlobalScan(t *testing.T) {
	svc, root := isolatedService(t)
	w, err := newFileWatcher([]string{root}, svc.shouldTrackPathChange, svc.metrics)
	if err != nil {
		t.Fatal(err)
	}
	svc.watcher = w
	stop := runIsolated(t, svc)
	if err := os.Mkdir(filepath.Join(root, "ordinary-empty-directory"), 0700); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 7*time.Second, "ordinary directory indexed", func() bool { m := svc.metrics.Snapshot(); return m.Inserted > 0 || m.ScanRuns > 0 })
	if m := svc.metrics.Snapshot(); m.ScanRuns != 0 || m.WatcherOverflows != 0 {
		t.Fatalf("ordinary mkdir requested global recovery: %+v", m)
	}
	stop()
	assertScopedEntries(t, svc, root, 2)
}

func TestMovedDirectoryDiscoversOnlyItsSubtree(t *testing.T) {
	svc, root := isolatedService(t)
	w := testWatcher(svc, 16)
	source := filepath.Join(t.TempDir(), "source")
	for _, dir := range []string{source, filepath.Join(source, "nested"), filepath.Join(source, ".git")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a.txt", "nested/b.txt", ".git/ignored.txt"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(root, "incoming")
	if err := os.Rename(source, destination); err != nil {
		t.Fatal(err)
	}
	// Deliver only the directory notification; no child events are assumed.
	w.nativeEvent(destination, 0x20000|0x800)
	stop := runIsolated(t, svc)
	waitUntil(t, 7*time.Second, "moved subtree indexed", func() bool { m := svc.metrics.Snapshot(); return m.Inserted >= 4 || m.ScanRuns > 0 })
	if m := svc.metrics.Snapshot(); m.ScanRuns != 0 || m.WatcherOverflows != 0 || m.Inserted != 4 {
		t.Fatalf("directory move was not scoped: %+v", m)
	}
	stop()
	assertScopedEntries(t, svc, root, 5)
}
