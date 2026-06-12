package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rjeczalik/notify"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/daemonstate"
	"omnia-search-tui/internal/indexer"
	"omnia-search-tui/internal/progress"
	"omnia-search-tui/internal/scanner"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
)

type Service struct {
	cfg     config.Config
	store   store.Backend
	scanner *scanner.Scanner
	indexer *indexer.Indexer
	logger  *log.Logger
}

const watchEventMask = notify.Create | notify.Write | notify.Remove | notify.Rename

const statusHeartbeatInterval = 15 * time.Second

func New(cfg config.Config, logger *log.Logger) (*Service, error) {
	st, err := store.OpenWithBackend(cfg.IndexDBPath, cfg.StoreBackend)
	if err != nil {
		return nil, err
	}
	scan := scanner.New(cfg.ExcludeGlobs)
	idx := indexer.New(cfg, scan, st, logger)
	return &Service{cfg: cfg, store: st, scanner: scan, indexer: idx, logger: logger}, nil
}

func (s *Service) Close() error {
	s.indexer.Stop()
	return s.store.Close()
}

func (s *Service) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.DaemonDir, 0o755); err != nil {
		return err
	}

	s.logger.Printf("daemon started | db=%s | status=%s | trigger=%s", s.cfg.IndexDBPath, s.cfg.DaemonStatusPath(), s.cfg.DaemonTriggerPath())

	indexedTotal := 0
	needsRecount := false
	snapshotDirty := true
	snapshotSeq := int64(0)
	lastCountAt := time.Now()
	hasWrittenStatus := false
	lastWrittenStatus := daemonstate.Status{}
	lastStatusWrite := time.Time{}

	count, err := s.store.Count(ctx)
	if err == nil {
		indexedTotal = count
		s.logger.Printf("initial indexed entries: %d", count)
	}

	initialStatus := s.buildStatus(indexedTotal, snapshotSeq)
	if err := s.writeStatus(initialStatus); err != nil {
		s.logger.Printf("write initial status failed: %v", err)
	} else {
		hasWrittenStatus = true
		lastWrittenStatus = initialStatus
		lastStatusWrite = time.Now()
	}
	if err == nil && count == 0 {
		s.logger.Printf("index is empty, requesting initial full reindex")
		if err := daemonstate.RequestFreshReindex(s.cfg.DaemonFreshStartPath()); err != nil {
			s.logger.Printf("request initial fresh reindex failed: %v", err)
		}
		if err := daemonstate.TriggerReindex(s.cfg.DaemonTriggerPath()); err != nil {
			s.logger.Printf("request initial reindex trigger failed: %v", err)
		}
	}

	if !s.indexer.IsRunning() {
		if err := s.publishReadonlySnapshot(ctx); err != nil {
			s.logger.Printf("publish readonly snapshot failed: %v", err)
		} else {
			snapshotDirty = false
			snapshotSeq++
		}
	} else {
		s.logger.Printf("skipping readonly snapshot publish while initial indexing is running")
	}

	events := make(chan notify.EventInfo, 8192)
	for _, root := range s.cfg.IncludePaths {
		watchPath := filepath.Clean(root) + "/..."
		if err := notify.Watch(watchPath, events, watchEventMask); err != nil {
			s.logger.Printf("watch setup failed for %s: %v", root, err)
		} else {
			s.logger.Printf("watching %s", watchPath)
		}
	}
	defer notify.Stop(events)

	pending := make(map[string]struct{}, 4096)
	flushTicker := time.NewTicker(900 * time.Millisecond)
	statusTicker := time.NewTicker(1 * time.Second)
	triggerTicker := time.NewTicker(1 * time.Second)
	defer flushTicker.Stop()
	defer statusTicker.Stop()
	defer triggerTicker.Stop()

	lastIndexing := s.indexer.IsRunning()
	lastProgressLog := time.Now()
	pendingFreshStart := false

	for {
		select {
		case <-ctx.Done():
			s.logger.Printf("shutdown requested, stopping daemon")
			if s.indexer.IsRunning() {
				if err := daemonstate.TriggerReindex(s.cfg.DaemonTriggerPath()); err != nil {
					s.logger.Printf("failed to persist interrupted reindex trigger: %v", err)
				} else {
					s.logger.Printf("persisted interrupted reindex trigger for next daemon start")
				}
			}
			finalStatus := s.buildStatus(indexedTotal, snapshotSeq)
			finalStatus.Running = false
			_ = s.writeStatus(finalStatus)
			return nil
		case e := <-events:
			if e != nil {
				path := filepath.Clean(e.Path())
				if !s.isDaemonManagedPath(path) {
					pending[path] = struct{}{}
				}
			}
		case <-flushTicker.C:
			if s.indexer.IsRunning() {
				continue
			}
			stats, err := s.flushPending(ctx, pending)
			if err != nil {
				s.logger.Printf("flush pending failed: %v", err)
			} else if stats.Total > 0 {
				s.logger.Printf("incremental flush | total=%d upserts=%d deletes=%d skipped=%d failures=%d", stats.Total, stats.Upserts, stats.Deletes, stats.Skipped, stats.Failures)
				if stats.Upserts > 0 || stats.Deletes > 0 {
					needsRecount = true
					snapshotDirty = true
				}
			}
		case <-triggerTicker.C:
			freshRequested, err := daemonstate.ConsumeFreshReindex(s.cfg.DaemonFreshStartPath())
			if err != nil {
				s.logger.Printf("consume fresh reindex signal failed: %v", err)
			}
			if freshRequested {
				pendingFreshStart = true
				s.logger.Printf("fresh reindex requested")
			}

			stopRequested, err := daemonstate.ConsumeReindexStop(s.cfg.DaemonStopPath())
			if err != nil {
				s.logger.Printf("consume stop reindex signal failed: %v", err)
			}
			if stopRequested && s.indexer.IsRunning() {
				s.logger.Printf("stop reindex requested")
				s.indexer.Stop()
				snapshotDirty = true
				continue
			}

			if pendingFreshStart {
				if s.indexer.IsRunning() {
					s.indexer.Stop()
					snapshotDirty = true
					continue
				}
				if err := s.startReindexFromScratch(ctx, "fresh reindex request"); err != nil {
					s.logger.Printf("fresh reindex failed: %v", err)
				} else {
					pendingFreshStart = false
				}
				continue
			}

			triggered, err := daemonstate.ConsumeTrigger(s.cfg.DaemonTriggerPath())
			if err != nil {
				s.logger.Printf("consume trigger failed: %v", err)
				continue
			}
			if triggered {
				if s.indexer.IsRunning() {
					continue
				}
				if err := s.startReindex(ctx, "reindex trigger received"); err != nil {
					s.logger.Printf("triggered reindex failed: %v", err)
				}
			}
		case <-statusTicker.C:
			idx := s.indexer.CurrentStatus()
			if shouldRefreshIndexedTotal(idx.Running, needsRecount, lastCountAt, time.Now()) {
				if total, err := s.store.Count(ctx); err == nil {
					indexedTotal = total
					if !idx.Running {
						needsRecount = false
					}
					lastCountAt = time.Now()
				}
			}
			if idx.Running && (!lastIndexing || time.Since(lastProgressLog) >= 3*time.Second) {
				s.logger.Printf("indexing progress | scanned=%d current=%s", idx.Scanned, trimMiddle(idx.CurrentPath, 90))
				lastProgressLog = time.Now()
			}
			if lastIndexing && !idx.Running {
				total, _ := s.store.Count(ctx)
				indexedTotal = total
				needsRecount = false
				lastCountAt = time.Now()
				snapshotDirty = true
				s.logger.Printf("indexing finished | total_indexed=%d last_error=%s", total, idx.LastError)
			}
			if !idx.Running && snapshotDirty {
				if err := s.publishReadonlySnapshot(ctx); err != nil {
					s.logger.Printf("publish readonly snapshot failed: %v", err)
				} else {
					snapshotDirty = false
					snapshotSeq++
				}
			}
			lastIndexing = idx.Running
			st := s.buildStatus(indexedTotal, snapshotSeq)
			shouldWrite := !hasWrittenStatus || !statusEqual(lastWrittenStatus, st) || time.Since(lastStatusWrite) >= statusHeartbeatInterval
			if !shouldWrite {
				continue
			}
			if err := s.writeStatus(st); err != nil {
				s.logger.Printf("write status failed: %v", err)
			} else {
				hasWrittenStatus = true
				lastWrittenStatus = st
				lastStatusWrite = time.Now()
			}
		}
	}
}

func (s *Service) startReindex(ctx context.Context, reason string) error {
	s.logger.Printf("%s", reason)
	return s.indexer.StartReindex(ctx)
}

func (s *Service) startReindexFromScratch(ctx context.Context, reason string) error {
	if err := daemonstate.ClearResumeState(s.cfg.DaemonResumeStatePath()); err != nil {
		return err
	}
	return s.startReindex(ctx, reason)
}

func shouldRefreshIndexedTotal(indexingRunning bool, needsRecount bool, lastCountAt time.Time, now time.Time) bool {
	if now.Sub(lastCountAt) < 5*time.Second {
		return false
	}
	return indexingRunning || needsRecount
}

type flushStats struct {
	Total    int
	Upserts  int
	Deletes  int
	Skipped  int
	Failures int
}

func (s *Service) flushPending(ctx context.Context, pending map[string]struct{}) (flushStats, error) {
	stats := flushStats{}
	if len(pending) == 0 {
		return stats, nil
	}
	paths := make([]string, 0, len(pending))
	for p := range pending {
		paths = append(paths, p)
	}
	stats.Total = len(paths)
	for _, path := range paths {
		delete(pending, path)
		if s.isDaemonManagedPath(path) {
			stats.Skipped++
			continue
		}
		if s.scanner.ShouldExclude(path) {
			stats.Skipped++
			continue
		}
		root := rootForPath(s.cfg.IncludePaths, path)
		if root == "" {
			stats.Skipped++
			continue
		}
		deleted, err := s.applyPathChange(ctx, path, root)
		if err != nil {
			stats.Failures++
			s.logger.Printf("apply path change %s failed: %v", path, err)
			continue
		}
		if deleted {
			stats.Deletes++
		} else {
			stats.Upserts++
		}
	}
	return stats, nil
}

func (s *Service) applyPathChange(ctx context.Context, path, root string) (bool, error) {
	_, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := s.store.DeletePathPrefix(ctx, path); err != nil {
				return false, err
			}
			return true, nil
		}
		if os.IsPermission(err) {
			return false, nil
		}
		return false, err
	}

	entry, err := s.scanner.EntryFromPath(path, root)
	if err != nil {
		if os.IsPermission(err) {
			return false, nil
		}
		return false, err
	}
	if err := s.store.UpsertEntry(ctx, entry); err != nil {
		return false, err
	}
	return false, nil
}
func (s *Service) buildStatus(indexedTotal int, snapshotSeq int64) daemonstate.Status {
	idx := s.indexer.CurrentStatus()
	return daemonstate.Status{
		Running:      true,
		Indexing:     idx.Running,
		Scanned:      idx.Scanned,
		CurrentPath:  idx.CurrentPath,
		PathProgress: idx.PathProgress,
		LastScanAt:   idx.FinishedAt,
		LastError:    idx.LastError,
		IndexedTotal: indexedTotal,
		SnapshotSeq:  snapshotSeq,
	}

}

func (s *Service) writeStatus(st daemonstate.Status) error {
	return daemonstate.Write(s.cfg.DaemonStatusPath(), st)
}

func statusEqual(a, b daemonstate.Status) bool {
	return a.Running == b.Running &&
		a.Indexing == b.Indexing &&
		a.Scanned == b.Scanned &&
		a.CurrentPath == b.CurrentPath &&
		pathProgressEqual(a.PathProgress, b.PathProgress) &&
		a.LastScanAt.Equal(b.LastScanAt) &&
		a.LastError == b.LastError &&
		a.IndexedTotal == b.IndexedTotal &&
		a.SnapshotSeq == b.SnapshotSeq
}

func pathProgressEqual(a, b []progress.PathProgress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Root != b[i].Root ||
			a[i].Scanned != b[i].Scanned ||
			a[i].EstimatedTotal != b[i].EstimatedTotal ||
			a[i].CurrentPath != b[i].CurrentPath ||
			a[i].Percent != b[i].Percent {
			return false
		}
	}
	return true
}

func (s *Service) isDaemonManagedPath(path string) bool {
	cleanPath := filepath.Clean(path)
	daemonDir := filepath.Clean(s.cfg.DaemonDir)
	if cleanPath == daemonDir {
		return true
	}
	prefix := daemonDir + string(os.PathSeparator)
	return strings.HasPrefix(cleanPath, prefix)
}

func rootForPath(roots []string, path string) string {
	cleanPath := filepath.Clean(path)
	best := ""
	for _, root := range roots {
		r := filepath.Clean(root)
		if cleanPath == r || strings.HasPrefix(cleanPath, r+string(os.PathSeparator)) {
			if len(r) > len(best) {
				best = r
			}
		}
	}
	return best
}

func (s *Service) readonlyIndexPath() string {
	return s.cfg.IndexDBPath + ".readonly"
}

func (s *Service) publishReadonlySnapshot(ctx context.Context) error {
	const maxAttempts = 5
	const baseDelay = 120 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := s.publishReadonlySnapshotOnce()
		if err == nil {
			if err := s.publishStartupPreviewCache(ctx); err != nil {
				s.logger.Printf("publish startup preview cache failed: %v", err)
			}
			return nil
		}
		lastErr = err
		if !isRetryableSnapshotError(err) || attempt == maxAttempts {
			return err
		}
		time.Sleep(time.Duration(attempt) * baseDelay)
	}
	return lastErr
}

func (s *Service) publishStartupPreviewCache(ctx context.Context) error {
	limit := startupcache.EffectiveLimit(s.cfg.MaxResults)
	sortSpec := sorter.SortSpec{
		Column:    sorter.Column(s.cfg.SortColumn),
		Direction: sorter.Direction(s.cfg.SortDirection),
	}
	res, err := s.store.Preview(ctx, sortSpec, limit)
	if err != nil {
		return err
	}
	return startupcache.Save(startupcache.Path(s.cfg), sortSpec, limit, res)
}

func (s *Service) publishReadonlySnapshotOnce() error {
	src := filepath.Clean(s.cfg.IndexDBPath)
	dst := filepath.Clean(s.readonlyIndexPath())
	tmp := dst + ".tmp"

	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := copyDir(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

func isRetryableSnapshotError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file") || strings.Contains(msg, "no such file or directory")
}

func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", src)
	}

	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}

	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}

		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		info, err := d.Info()
		if err != nil {
			return err
		}
		dstFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(dstFile, srcFile); err != nil {
			_ = dstFile.Close()
			return err
		}
		return dstFile.Close()
	})
}

func RunMain(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logPath := filepath.Join(cfg.DaemonDir, "daemon.log")
	if err := os.MkdirAll(cfg.DaemonDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer f.Close()
	logger := log.New(io.MultiWriter(os.Stdout, f), "", log.LstdFlags)

	svc, err := New(cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	return svc.Run(ctx)
}

func trimMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := (max - 3) / 2
	if half < 1 {
		return s[:max]
	}
	return s[:half] + "..." + s[len(s)-half:]
}
