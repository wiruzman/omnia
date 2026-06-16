package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/rjeczalik/notify"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/daemonstate"
	"omnia-search-tui/internal/indexer"
	"omnia-search-tui/internal/logging"
	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/progress"
	"omnia-search-tui/internal/scanner"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
)

type Service struct {
	cfg            config.Config
	store          store.Backend
	scanner        *scanner.Scanner
	indexer        *indexer.Indexer
	logger         logging.PrintfLogger
	startupPreview startupPreviewState
	now            func() time.Time
}

const watchEventMask = notify.Create | notify.Write | notify.Remove | notify.Rename

const statusHeartbeatInterval = 15 * time.Second
const incrementalFlushDebounce = 3 * time.Second
const startupPreviewMinInterval = time.Minute

type startupPreviewState struct {
	sortSpec    sorter.SortSpec
	limit       int
	entries     []model.Entry
	paths       map[string]struct{}
	publishedAt time.Time
}

func New(cfg config.Config, logger logging.PrintfLogger) (*Service, error) {
	st, err := store.OpenSQLite(cfg.IndexDBPath)
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

	indexedTotal := s.lastIndexedTotal()
	snapshotSeq := int64(0)
	hasWrittenStatus := false
	lastWrittenStatus := daemonstate.Status{}
	lastStatusWrite := time.Time{}
	startupPreviewDirty := false
	forceStartupPreviewPublish := false

	hasEntries, err := s.store.HasEntries(ctx)
	if err != nil {
		s.logger.Printf("check initial index state failed: %v", err)
	} else {
		s.logger.Printf("initial indexed entries estimate: %d", indexedTotal)
	}

	initialStatus := s.buildStatus(indexedTotal, snapshotSeq)
	if err := s.writeStatus(initialStatus); err != nil {
		s.logger.Printf("write initial status failed: %v", err)
	} else {
		hasWrittenStatus = true
		lastWrittenStatus = initialStatus
		lastStatusWrite = time.Now()
	}
	if err == nil && !hasEntries {
		s.logger.Printf("index is empty, requesting initial full reindex")
		if err := daemonstate.RequestFreshReindex(s.cfg.DaemonFreshStartPath()); err != nil {
			s.logger.Printf("request initial fresh reindex failed: %v", err)
		}
		if err := daemonstate.TriggerReindex(s.cfg.DaemonTriggerPath()); err != nil {
			s.logger.Printf("request initial reindex trigger failed: %v", err)
		}
	}

	if !s.indexer.IsRunning() {
		if err := s.publishStartupPreviewCache(ctx); err != nil {
			s.logger.Printf("publish startup preview cache failed: %v", err)
		}
	} else {
		s.logger.Printf("skipping startup preview cache publish while initial indexing is running")
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
	flushTimer := time.NewTimer(time.Hour)
	stopTimer(flushTimer)
	statusTicker := time.NewTicker(1 * time.Second)
	triggerTicker := time.NewTicker(1 * time.Second)
	defer flushTimer.Stop()
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
				if s.shouldTrackPathChange(path) {
					pending[path] = struct{}{}
					resetTimer(flushTimer, incrementalFlushDebounce)
				}
			}
		case <-flushTimer.C:
			if s.indexer.IsRunning() {
				continue
			}
			stats, err := s.flushPending(ctx, pending)
			if err != nil {
				s.logger.Printf("flush pending failed: %v", err)
			} else if stats.Total > 0 {
				s.logger.Printf("incremental flush | total=%d upserts=%d deletes=%d skipped=%d failures=%d", stats.Total, stats.Upserts, stats.Deletes, stats.Skipped, stats.Failures)
				if stats.hasChanges() {
					indexedTotal = applyIndexDelta(indexedTotal, stats.IndexDelta)
					snapshotSeq++
					if stats.PreviewRelevant {
						startupPreviewDirty = true
					}
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
				continue
			}

			if pendingFreshStart {
				if s.indexer.IsRunning() {
					s.indexer.Stop()
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
			if idx.Running && (!lastIndexing || time.Since(lastProgressLog) >= 3*time.Second) {
				s.logger.Printf("indexing progress | scanned=%d current=%s", idx.Scanned, trimMiddle(idx.CurrentPath, 90))
				lastProgressLog = time.Now()
			}
			if lastIndexing && !idx.Running {
				total, _ := s.store.Count(ctx)
				indexedTotal = total
				snapshotSeq++
				startupPreviewDirty = true
				forceStartupPreviewPublish = true
				s.logger.Printf("indexing finished | total_indexed=%d last_error=%s", total, idx.LastError)
				if len(pending) > 0 {
					resetTimer(flushTimer, incrementalFlushDebounce)
				}
			}
			now := s.currentTime()
			if !idx.Running && s.shouldPublishStartupPreview(startupPreviewDirty, forceStartupPreviewPublish, now) {
				if err := s.publishStartupPreviewCache(ctx); err != nil {
					s.logger.Printf("publish startup preview cache failed: %v", err)
				} else {
					startupPreviewDirty = false
					forceStartupPreviewPublish = false
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

func (s *Service) lastIndexedTotal() int {
	st, err := daemonstate.Read(s.cfg.DaemonStatusPath())
	if err != nil || st.IndexedTotal < 0 {
		return 0
	}
	return st.IndexedTotal
}

func applyIndexDelta(total int, delta int) int {
	total += delta
	if total < 0 {
		return 0
	}
	return total
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	stopTimer(timer)
	timer.Reset(delay)
}

func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

type flushStats struct {
	Total           int
	Upserts         int
	Deletes         int
	Skipped         int
	Failures        int
	IndexDelta      int
	PreviewRelevant bool
}

func (s flushStats) hasChanges() bool {
	return s.Upserts > 0 || s.Deletes > 0
}

type pathChangeResult struct {
	Upserted   int
	Deleted    int
	IndexDelta int
	Entry      model.Entry
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
		if !s.shouldTrackPathChange(path) {
			stats.Skipped++
			continue
		}
		result, err := s.applyPathChange(ctx, path, rootForPath(s.cfg.IncludePaths, path))
		if err != nil {
			stats.Failures++
			s.logger.Printf("apply path change %s failed: %v", path, err)
			continue
		}
		if result.Deleted > 0 {
			stats.Deletes += result.Deleted
			stats.IndexDelta -= result.Deleted
			if s.deletedPathMayAffectStartupPreview(path) {
				stats.PreviewRelevant = true
			}
			continue
		}
		if result.Upserted > 0 {
			stats.Upserts += result.Upserted
			stats.IndexDelta += result.IndexDelta
			if s.entryMayAffectStartupPreview(result.Entry) {
				stats.PreviewRelevant = true
			}
			continue
		}
		stats.Skipped++
	}
	return stats, nil
}

func (s *Service) applyPathChange(ctx context.Context, path, root string) (pathChangeResult, error) {
	entry, err := s.scanner.EntryFromPath(path, root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			deleted, err := s.store.DeletePathPrefixCount(ctx, path)
			if err != nil {
				return pathChangeResult{}, err
			}
			return pathChangeResult{Deleted: deleted}, nil
		}
		if os.IsPermission(err) {
			return pathChangeResult{}, nil
		}
		return pathChangeResult{}, err
	}

	result, err := s.store.UpsertEntryIfChanged(ctx, entry)
	if err != nil {
		return pathChangeResult{}, err
	}
	if !result.Changed {
		return pathChangeResult{}, nil
	}
	indexDelta := 0
	if result.Inserted {
		indexDelta = 1
	}
	return pathChangeResult{Upserted: 1, IndexDelta: indexDelta, Entry: entry}, nil
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

func (s *Service) shouldTrackPathChange(path string) bool {
	if s.isDaemonManagedPath(path) {
		return false
	}
	if s.scanner != nil && s.scanner.ShouldExclude(path) {
		return false
	}
	return rootForPath(s.cfg.IncludePaths, path) != ""
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

func (s *Service) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Service) shouldPublishStartupPreview(dirty bool, force bool, now time.Time) bool {
	if !dirty {
		return false
	}
	sortSpec := s.startupPreviewSortSpec()
	limit := startupcache.EffectiveLimit(s.cfg.MaxResults)
	if force || s.startupPreview.publishedAt.IsZero() || s.startupPreview.sortSpec != sortSpec || s.startupPreview.limit != limit {
		return true
	}
	return now.Sub(s.startupPreview.publishedAt) >= startupPreviewMinInterval
}

func (s *Service) publishStartupPreviewCache(ctx context.Context) error {
	limit := startupcache.EffectiveLimit(s.cfg.MaxResults)
	sortSpec := s.startupPreviewSortSpec()
	res, err := s.store.Preview(ctx, sortSpec, limit)
	if err != nil {
		return err
	}
	if err := startupcache.Save(startupcache.Path(s.cfg), sortSpec, limit, res); err != nil {
		return err
	}
	s.setStartupPreviewState(sortSpec, limit, res)
	return nil
}

func (s *Service) startupPreviewSortSpec() sorter.SortSpec {
	return sorter.SortSpec{
		Column:    sorter.Column(s.cfg.SortColumn),
		Direction: sorter.Direction(s.cfg.SortDirection),
	}
}

func (s *Service) setStartupPreviewState(sortSpec sorter.SortSpec, limit int, result store.QueryResult) {
	entries := append([]model.Entry(nil), result.Entries...)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	paths := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		paths[filepath.Clean(entry.Path)] = struct{}{}
	}
	s.startupPreview = startupPreviewState{
		sortSpec:    sortSpec,
		limit:       limit,
		entries:     entries,
		paths:       paths,
		publishedAt: s.currentTime(),
	}
}

func (s *Service) entryMayAffectStartupPreview(entry model.Entry) bool {
	state := s.startupPreview
	sortSpec := s.startupPreviewSortSpec()
	limit := startupcache.EffectiveLimit(s.cfg.MaxResults)
	if state.publishedAt.IsZero() || state.sortSpec != sortSpec || state.limit != limit {
		return true
	}
	if _, ok := state.paths[filepath.Clean(entry.Path)]; ok {
		return true
	}
	if len(state.entries) < state.limit {
		return true
	}
	if len(state.entries) == 0 {
		return true
	}
	boundary := state.entries[len(state.entries)-1]
	return startupcache.CompareEntries(entry, boundary, state.sortSpec) <= 0
}

func (s *Service) deletedPathMayAffectStartupPreview(path string) bool {
	state := s.startupPreview
	sortSpec := s.startupPreviewSortSpec()
	limit := startupcache.EffectiveLimit(s.cfg.MaxResults)
	if state.publishedAt.IsZero() || state.sortSpec != sortSpec || state.limit != limit {
		return true
	}
	if len(state.entries) == 0 {
		return false
	}
	cleanPath := filepath.Clean(path)
	prefix := cleanPath + string(os.PathSeparator)
	for previewPath := range state.paths {
		if previewPath == cleanPath || strings.HasPrefix(previewPath, prefix) {
			return true
		}
	}
	return false
}

func RunMain(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	daemonLog, err := logging.OpenDaemon(cfg)
	if err != nil {
		return err
	}
	logger := daemonLog.Logger
	defer func() { _ = daemonLog.Close() }()
	defer logPanic(logger)

	logger.Printf("daemon logging initialized | path=%s level=%s max_bytes=%d backups=%d stdout=%t", daemonLog.Path, cfg.DaemonLogLevel, cfg.DaemonLogMaxBytes, cfg.DaemonLogBackups, daemonLog.ToStdout)
	if err := lowerDaemonPriority(); err != nil {
		logger.Printf("lower daemon priority failed: %v", err)
	} else {
		logger.Printf("daemon priority lowered for background indexing")
	}

	svc, err := New(cfg, logger)
	if err != nil {
		logger.Printf("daemon initialization failed: %v", err)
		return err
	}
	defer func() { _ = svc.Close() }()
	if err := svc.Run(ctx); err != nil {
		logger.Printf("daemon stopped with error: %v", err)
		return err
	}
	logger.Printf("daemon stopped")
	return nil
}

func logPanic(logger logging.PrintfLogger) {
	if r := recover(); r != nil {
		logger.Printf("daemon panic: %v | stack=%s", r, strings.TrimSpace(string(debug.Stack())))
		panic(r)
	}
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
