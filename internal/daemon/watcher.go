package daemon

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/wiruzman/omnia/internal/metrics"
)

const watcherCapacity = 8192

type fileEvent struct {
	path     string
	discover bool // Discover descendants for incoming directories or scoped recovery.
	scoped   bool // Numeric accounting only; uses the same pending work as discovery.
}

// fileWatcher holds only a bounded channel, never a history of observed paths.
// Lost events are coalesced into an explicit request for full reconciliation.
type fileWatcher struct {
	events   chan fileEvent
	overflow chan struct{}
	accept   func(string) bool
	metrics  *metrics.Counters
	roots    []string
	stop     func()
	once     sync.Once
}

func newFileWatcher(roots []string, accept func(string) bool, counters *metrics.Counters) (*fileWatcher, error) {
	w := &fileWatcher{events: make(chan fileEvent, watcherCapacity), overflow: make(chan struct{}, 1), accept: accept, metrics: counters, roots: append([]string(nil), roots...)}
	stop, err := startNativeWatcher(roots, w)
	if err != nil {
		return nil, err
	}
	w.stop = stop
	return w, nil
}

func (w *fileWatcher) Close() {
	w.once.Do(func() {
		if w.stop != nil {
			w.stop()
		}
	})
}

func (w *fileWatcher) lost() {
	select {
	case w.overflow <- struct{}{}:
		w.metrics.WatcherOverflow()
	default:
	}
}

func (w *fileWatcher) publish(path string) {
	w.publishEvent(fileEvent{path: path})
}

// Coalesced native events can name an ancestor of a configured root. Reconcile
// those roots, not unrelated trees and not merely the ancestor's direct children.
func (w *fileWatcher) recoverPath(path string) {
	path = filepath.Clean(path)
	if w.accept(path) {
		w.publishEvent(fileEvent{path: path, discover: true, scoped: true})
		return
	}
	matched := false
	for _, root := range w.roots {
		root = filepath.Clean(root)
		if path != root && pathWithin(path, root) {
			w.publishEvent(fileEvent{path: root, discover: true, scoped: true})
			matched = true
		}
	}
	if !matched {
		w.publishEvent(fileEvent{path: path, discover: true, scoped: true})
	}
}

func pathWithin(root, path string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

func (w *fileWatcher) publishEvent(event fileEvent) {
	event.path = filepath.Clean(event.path)
	accepted := w.accept(event.path)
	w.metrics.Event(accepted)
	if !accepted {
		return
	}
	if event.scoped {
		w.metrics.ScopedRecoveryRequested()
	}
	select {
	case w.events <- event:
	default:
		w.metrics.RecoveryEvent(metrics.DeliveryOverflow)
		w.lost()
	}
}
