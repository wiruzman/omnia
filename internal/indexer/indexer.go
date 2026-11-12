package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/daemonstate"
	"github.com/wiruzman/omnia/internal/logging"
	"github.com/wiruzman/omnia/internal/metrics"
	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/progress"
	"github.com/wiruzman/omnia/internal/scanner"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/startupcache"
	"github.com/wiruzman/omnia/internal/store"
)

const startupCacheSaveInterval = time.Second

type Status struct {
	Running      bool
	Scanned      int64
	CurrentPath  string
	PathProgress []progress.PathProgress
	StartedAt    time.Time
	LastError    string
	FinishedAt   time.Time
}

type Indexer struct {
	cfg      config.Config
	scanner  *scanner.Scanner
	store    store.Backend
	logger   logging.PrintfLogger
	resumeAt string
	status   atomic.Pointer[Status]
	mu       sync.Mutex
	cancelFn context.CancelFunc
	done     chan struct{}
	metrics  *metrics.Counters
}

func New(cfg config.Config, scan *scanner.Scanner, st store.Backend, logger logging.PrintfLogger) *Indexer {
	return NewWithMetrics(cfg, scan, st, logger, nil)
}

func NewWithMetrics(cfg config.Config, scan *scanner.Scanner, st store.Backend, logger logging.PrintfLogger, counters *metrics.Counters) *Indexer {
	initial := &Status{}
	idx := &Indexer{cfg: cfg, scanner: scan, store: st, logger: logger, resumeAt: cfg.DaemonResumeStatePath(), metrics: counters}
	idx.status.Store(initial)
	return idx
}

func (i *Indexer) CurrentStatus() Status {
	st := i.status.Load()
	if st == nil {
		return Status{}
	}
	return *st
}

func (i *Indexer) IsRunning() bool {
	return i.CurrentStatus().Running
}

func (i *Indexer) StartReindex(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cancelFn != nil {
		return fmt.Errorf("indexing is already running")
	}

	runCtx, cancel := context.WithCancel(ctx)
	i.cancelFn = cancel
	i.done = make(chan struct{})
	now := time.Now()
	i.status.Store(&Status{Running: true, StartedAt: now})
	i.metrics.ScanStarted()

	go func() {
		var scanned uint64
		var reported uint64
		defer func() {
			i.metrics.ScanFinished(scanned-reported, time.Since(now))
			i.mu.Lock()
			i.cancelFn = nil
			close(i.done)
			i.mu.Unlock()
		}()

		scanID := newScanID()
		walkOptions := scanner.WalkOptions{
			ThrottleEvery: i.cfg.ScanThrottleEvery,
			ThrottleDelay: time.Duration(i.cfg.ScanThrottleMs) * time.Millisecond,
			SkipPath:      i.cfg.IsRuntimePath,
			OnUnreadable: func(path string) error {
				// Store protection with the scan marker itself, so it survives
				// interruption/resume without a second path-history file.
				return i.store.MarkSubtreeSeen(runCtx, scanID, path)
			},
		}
		resume, err := daemonstate.ReadResumeState(i.resumeAt)
		if err == nil && resume.ScanID > 0 {
			scanID = resume.ScanID
			walkOptions.ResumeRoot = resume.Root
			walkOptions.ResumeAfterPath = resume.CurrentPath
			i.logger.Printf("resuming reindex | scan_id=%d root=%s path=%s", scanID, resume.Root, resume.CurrentPath)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			i.logger.Printf("resume state read failed: %v", err)
		}

		buildStartupCache := walkOptions.ResumeRoot == "" && walkOptions.ResumeAfterPath == ""
		sortSpec := sorter.SortSpec{
			Column:    sorter.Column(i.cfg.SortColumn),
			Direction: sorter.Direction(i.cfg.SortDirection),
		}
		startupCacheLimit := startupcache.EffectiveLimit(i.cfg.MaxResults)
		startupTop := startupcache.NewTop(sortSpec, startupCacheLimit)
		lastStartupCacheSave := time.Now()
		hasSavedStartupCache := false
		emitted := int64(0)
		saveStartupCache := func(force bool) {
			if !buildStartupCache || startupTop.Len() == 0 {
				return
			}
			now := time.Now()
			if !force {
				if hasSavedStartupCache && now.Sub(lastStartupCacheSave) < startupCacheSaveInterval {
					return
				}
				if !hasSavedStartupCache && startupTop.Len() < startupCacheLimit && now.Sub(lastStartupCacheSave) < startupCacheSaveInterval {
					return
				}
			}
			result := startupTop.Result(int(emitted))
			started := time.Now()
			err := startupcache.Save(startupcache.Path(i.cfg), sortSpec, startupCacheLimit, result)
			i.metrics.Preview(time.Since(started), err != nil)
			if err != nil {
				i.logger.Printf("save startup cache during reindex failed: %v", err)
				return
			}
			lastStartupCacheSave = now
			hasSavedStartupCache = true
		}

		if err := i.store.BeginScan(runCtx, scanID); err != nil {
			i.finishWithError(err)
			return
		}
		if err := daemonstate.WriteResumeState(i.resumeAt, daemonstate.ReindexResumeState{ScanID: scanID}); err != nil {
			i.logger.Printf("resume state write failed: %v", err)
		}

		estimatedByRoot, err := i.store.CountByRoots(runCtx, i.cfg.IncludePaths)
		if err != nil {
			i.logger.Printf("count by roots failed: %v", err)
			estimatedByRoot = make(map[string]int64, len(i.cfg.IncludePaths))
		}

		pathProgress := make(map[string]progress.PathProgress, len(i.cfg.IncludePaths))
		for _, root := range i.cfg.IncludePaths {
			cleanRoot := filepath.Clean(root)
			est := estimatedByRoot[cleanRoot]
			pathProgress[cleanRoot] = progress.PathProgress{
				Root:           cleanRoot,
				Scanned:        0,
				EstimatedTotal: est,
				Percent:        progress.ClampPercent(0, est),
			}
		}

		batch := make([]model.Entry, 0, i.cfg.ScanBatchSize)
		emit := func(e model.Entry) error {
			if err := runCtx.Err(); err != nil {
				return err
			}
			scanned++
			if i.metrics != nil && scanned-reported >= uint64(max(i.cfg.ScanBatchSize, 1)) {
				i.metrics.AddScanned(scanned - reported)
				reported = scanned
			}
			if buildStartupCache {
				emitted++
				startupTop.Add(e)
				saveStartupCache(false)
			}
			batch = append(batch, e)
			if len(batch) >= i.cfg.ScanBatchSize {
				if err := i.store.UpsertBatch(runCtx, scanID, batch); err != nil {
					return err
				}
				batch = batch[:0]
			}
			return nil
		}
		lastRoot := walkOptions.ResumeRoot
		onProgress := func(p scanner.Progress) {
			st := i.CurrentStatus()
			st.Scanned = p.Scanned
			if p.Current != "" {
				st.CurrentPath = p.Current
			}
			if p.Root != "" {
				lastRoot = p.Root
				cleanRoot := filepath.Clean(p.Root)
				row := pathProgress[cleanRoot]
				if row.Root == "" {
					row.Root = cleanRoot
				}
				row.Scanned = p.RootScanned
				row.CurrentPath = p.Current
				if row.EstimatedTotal < row.Scanned {
					row.EstimatedTotal = row.Scanned
				}
				row.Percent = progress.ClampPercent(row.Scanned, row.EstimatedTotal)
				pathProgress[cleanRoot] = row
			}

			rows := make([]progress.PathProgress, 0, len(pathProgress))
			for _, row := range pathProgress {
				rows = append(rows, row)
			}
			sort.Slice(rows, func(i, j int) bool {
				return rows[i].Root < rows[j].Root
			})
			st.PathProgress = rows
			i.status.Store(&st)

			if p.Current != "" {
				if err := daemonstate.WriteResumeState(i.resumeAt, daemonstate.ReindexResumeState{
					ScanID:      scanID,
					Root:        filepath.Clean(lastRoot),
					CurrentPath: p.Current,
				}); err != nil {
					i.logger.Printf("resume state write failed: %v", err)
				}
			}
		}
		onWarn := func(err error) {
			i.logger.Printf("scanner warning: %v", err)
		}

		err = i.scanner.WalkWithOptions(i.cfg.IncludePaths, emit, onProgress, onWarn, walkOptions)
		if err != nil {
			i.finishWithError(err)
			return
		}
		if len(batch) > 0 {
			if err := i.store.UpsertBatch(runCtx, scanID, batch); err != nil {
				i.finishWithError(err)
				return
			}
		}
		saveStartupCache(true)
		if err := i.store.CleanupStale(runCtx, scanID, i.cfg.IncludePaths); err != nil {
			i.finishWithError(err)
			return
		}
		if err := daemonstate.ClearResumeState(i.resumeAt); err != nil {
			i.logger.Printf("resume state cleanup failed: %v", err)
		}

		st := i.CurrentStatus()
		st.Running = false
		st.FinishedAt = time.Now()
		i.status.Store(&st)
	}()

	return nil
}

func newScanID() int64 {
	return time.Now().UnixMicro()
}

func (i *Indexer) Stop() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cancelFn != nil {
		i.cancelFn()
	}
}

// Wait joins the current scan, including its completion metrics. Stop requests
// cancellation separately so interactive stop requests remain nonblocking.
func (i *Indexer) Wait() {
	i.mu.Lock()
	done := i.done
	i.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (i *Indexer) finishWithError(err error) {
	if errors.Is(err, context.Canceled) {
		i.logger.Printf("indexing interrupted")
		st := i.CurrentStatus()
		st.Running = false
		st.LastError = ""
		st.FinishedAt = time.Now()
		i.status.Store(&st)
		return
	}
	i.logger.Printf("indexing error: %v", err)
	i.metrics.ScanFailed()
	st := i.CurrentStatus()
	st.Running = false
	st.LastError = err.Error()
	st.FinishedAt = time.Now()
	i.status.Store(&st)
}
