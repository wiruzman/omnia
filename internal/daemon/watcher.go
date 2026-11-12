package daemon

import (
	"path/filepath"
	"sync"

	"github.com/wiruzman/omnia/internal/metrics"
)

const watcherCapacity = 8192

type fileEvent struct {
	path     string
	discover bool // Only directory creation/moves need descendant discovery.
}

// fileWatcher holds only a bounded channel, never a history of observed paths.
// Lost events are coalesced into an explicit request for full reconciliation.
type fileWatcher struct {
	events   chan fileEvent
	overflow chan struct{}
	accept   func(string) bool
	metrics  *metrics.Counters
	stop     func()
	once     sync.Once
}

func newFileWatcher(roots []string, accept func(string) bool, counters *metrics.Counters) (*fileWatcher, error) {
	w := &fileWatcher{events: make(chan fileEvent, watcherCapacity), overflow: make(chan struct{}, 1), accept: accept, metrics: counters}
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

func (w *fileWatcher) publishEvent(event fileEvent) {
	event.path = filepath.Clean(event.path)
	accepted := w.accept(event.path)
	w.metrics.Event(accepted)
	if !accepted {
		return
	}
	select {
	case w.events <- event:
	default:
		w.lost()
	}
}
