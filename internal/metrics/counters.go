// Package metrics records aggregate numeric measurements. It must not accept file
// entries, paths, search queries, arbitrary labels, or diagnostic error strings.
package metrics

import (
	"sync"
	"time"
)

// Work is an explicit allowlist. Counters are cumulative for this daemon run;
// gauges and fields ending in _max describe the current recording interval.
type Work struct {
	EventsReceived    uint64  `json:"events_received_total"`
	EventsAccepted    uint64  `json:"events_accepted_total"`
	EventsIgnored     uint64  `json:"events_ignored_total"`
	Pending           uint64  `json:"pending_entries"`
	PendingMax        uint64  `json:"pending_entries_max"`
	Indexed           uint64  `json:"indexed_entries"`
	Flushes           uint64  `json:"flushes_total"`
	Inserted          uint64  `json:"inserted_entries_total"`
	Updated           uint64  `json:"updated_entries_total"`
	Deleted           uint64  `json:"deleted_entries_total"`
	Unchanged         uint64  `json:"unchanged_entries_total"`
	Skipped           uint64  `json:"skipped_entries_total"`
	Failures          uint64  `json:"entry_failures_total"`
	FlushSeconds      float64 `json:"flush_seconds_total"`
	FlushMaxSeconds   float64 `json:"flush_seconds_max"`
	PreviewBuilds     uint64  `json:"preview_builds_total"`
	PreviewFailures   uint64  `json:"preview_failures_total"`
	PreviewSeconds    float64 `json:"preview_seconds_total"`
	PreviewMaxSeconds float64 `json:"preview_seconds_max"`
	ScanRuns          uint64  `json:"scan_runs_total"`
	ScanActive        uint64  `json:"active_scans"`
	Scanned           uint64  `json:"scanned_entries_total"`
	ScanFailures      uint64  `json:"scan_failures_total"`
	ScanSeconds       float64 `json:"scan_seconds_total"`
	ScanMaxSeconds    float64 `json:"scan_seconds_max"`
}

type Counters struct {
	mu   sync.Mutex
	work Work
}

// Snapshot returns a consistent numeric copy without resetting interval peaks.
func (c *Counters) Snapshot() Work {
	if c == nil {
		return Work{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.work
}

func (c *Counters) AddScanned(n uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.work.Scanned += n
}

// All mutation methods tolerate nil: disabled collection does no allocation or I/O.
func (c *Counters) Event(accepted bool, pending int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.work.EventsReceived++
	if accepted {
		c.work.EventsAccepted++
	} else {
		c.work.EventsIgnored++
	}
	c.pending(pending)
}

func (c *Counters) pending(n int) {
	c.work.Pending = uint64(max(n, 0))
	c.work.PendingMax = max(c.work.PendingMax, c.work.Pending)
}

func (c *Counters) SetPending(n int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending(n)
}

func (c *Counters) SetIndexed(n int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.work.Indexed = uint64(max(n, 0))
}

func (c *Counters) Flush(inserted, updated, deleted, unchanged, skipped, failures int, elapsed time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.work.Flushes++
	c.work.Inserted += uint64(max(inserted, 0))
	c.work.Updated += uint64(max(updated, 0))
	c.work.Deleted += uint64(max(deleted, 0))
	c.work.Unchanged += uint64(max(unchanged, 0))
	c.work.Skipped += uint64(max(skipped, 0))
	c.work.Failures += uint64(max(failures, 0))
	c.work.FlushSeconds += elapsed.Seconds()
	c.work.FlushMaxSeconds = max(c.work.FlushMaxSeconds, elapsed.Seconds())
}

func (c *Counters) Preview(elapsed time.Duration, failed bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.work.PreviewBuilds++
	if failed {
		c.work.PreviewFailures++
	}
	c.work.PreviewSeconds += elapsed.Seconds()
	c.work.PreviewMaxSeconds = max(c.work.PreviewMaxSeconds, elapsed.Seconds())
}

func (c *Counters) ScanStarted() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.work.ScanRuns++
	c.work.ScanActive++
}

func (c *Counters) ScanFinished(scanned uint64, elapsed time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.work.ScanActive > 0 {
		c.work.ScanActive--
	}
	c.work.Scanned += scanned
	c.work.ScanSeconds += elapsed.Seconds()
	c.work.ScanMaxSeconds = max(c.work.ScanMaxSeconds, elapsed.Seconds())
}

func (c *Counters) ScanFailed() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.work.ScanFailures++
}

func (c *Counters) take() Work {
	if c == nil {
		return Work{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.work
	c.work.PendingMax = c.work.Pending
	c.work.FlushMaxSeconds = 0
	c.work.PreviewMaxSeconds = 0
	c.work.ScanMaxSeconds = 0
	return w
}
