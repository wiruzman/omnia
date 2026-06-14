package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/daemonstate"
	"omnia-search-tui/internal/indexer"
)

const snapshotRefreshRetryInterval = 6 * time.Second

func (a *App) updateStatus() {
	st := a.currentIndexerStatus()
	indexText := "idle"
	if st.Running {
		indexText = fmt.Sprintf("indexing: %d scanned", st.Scanned)
		if st.CurrentPath != "" {
			indexText += " | " + trimMiddle(st.CurrentPath, 60)
		}
	} else if st.LastError != "" {
		indexText = "error: " + trimMiddle(st.LastError, 80)
	}
	query := a.input.GetText()
	indexedTotal := a.total
	if a.hasDaemonStatus {
		indexedTotal = a.lastDaemonStatus.IndexedTotal
	}
	countLabel := fmt.Sprintf("indexed: %d", indexedTotal)
	if strings.TrimSpace(query) != "" {
		countLabel = fmt.Sprintf("matches: %d", a.visible)
	}
	deleteText := ""
	if deleting, deletePath := a.deleteProgress(); deleting {
		frames := []string{"|", "/", "-", "\\"}
		frame := frames[(time.Now().UnixNano()/int64(200*time.Millisecond))%int64(len(frames))]
		deleteText = fmt.Sprintf(" | deleting: %s %s", frame, trimMiddle(deletePath, 60))
	}
	activityText := a.activityText()
	a.status.SetText(fmt.Sprintf("%s | visible: %d | activity: %s | sort: %s %s | %s%s | query: %s",
		countLabel, a.visible, activityText, a.sortSpec.Column, a.sortSpec.Direction, indexText, deleteText, query))
	a.renderProgressTable()
}

func (a *App) startStatusLoop(ctx context.Context) {
	ticker := time.NewTicker(400 * time.Millisecond)
	go func() {
		defer ticker.Stop()
		wasRunning := a.isIndexing()
		lastLiveRefresh := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.tui.QueueUpdateDraw(func() {
					running := a.isIndexing()
					now := time.Now()
					if !wasRunning && running {
						a.forgetEmptyQueryResults()
					}
					if shouldQueueLiveRefresh(running, a.query, lastLiveRefresh, now, a.searchState.Load()) {
						a.requestRefreshAsync(a.query, a.sortSpec)
						lastLiveRefresh = now
					}
					if wasRunning && !running {
						a.forgetEmptyQueryResults()
						a.requestRefreshAsync(a.query, a.sortSpec)
						lastLiveRefresh = time.Now()
					}
					wasRunning = running
					a.updateStatus()
				})
			}
		}
	}()
}

func shouldQueueLiveRefresh(running bool, query string, lastLiveRefresh, now time.Time, state uint32) bool {
	if !running || strings.TrimSpace(query) != "" {
		return false
	}
	if state == searchStatePending || state == searchStateRunning {
		return false
	}
	return now.Sub(lastLiveRefresh) >= 2*time.Second
}

func (a *App) isIndexing() bool {
	return a.currentIndexerStatus().Running
}

func (a *App) currentIndexerStatus() indexer.Status {
	st, err := daemonstate.Read(a.cfg.DaemonStatusPath())
	if err != nil {
		if !os.IsNotExist(err) {
			a.logger.Printf("read daemon status failed: %v", err)
		}
		if a.hasDaemonStatus {
			return indexer.Status{
				Running:      a.lastDaemonStatus.Indexing,
				Scanned:      a.lastDaemonStatus.Scanned,
				CurrentPath:  a.lastDaemonStatus.CurrentPath,
				PathProgress: a.lastDaemonStatus.PathProgress,
				LastError:    a.lastDaemonStatus.LastError,
				FinishedAt:   a.lastDaemonStatus.LastScanAt,
			}
		}
		return indexer.Status{}
	}
	a.lastDaemonStatus = st
	a.hasDaemonStatus = true
	a.maybeRefreshStoreConnection(st)
	return indexer.Status{
		Running:      st.Indexing,
		Scanned:      st.Scanned,
		CurrentPath:  st.CurrentPath,
		PathProgress: st.PathProgress,
		LastError:    st.LastError,
		FinishedAt:   st.LastScanAt,
	}
}

func (a *App) maybeRefreshStoreConnection(st daemonstate.Status) {
	if st.SnapshotSeq <= 0 {
		return
	}
	if st.SnapshotSeq == a.daemonSnapshotSeq {
		return
	}
	now := time.Now()
	if !shouldAttemptSnapshotRefresh(st.SnapshotSeq, a.snapshotRefreshAttemptSeq, a.lastSnapshotRefreshAttempt, now) {
		return
	}
	a.snapshotRefreshAttemptSeq = st.SnapshotSeq
	a.lastSnapshotRefreshAttempt = now
	if err := a.refreshStoreConnection(); err != nil {
		a.logger.Printf("refresh store connection failed: %v", err)
		return
	}
	a.daemonSnapshotSeq = st.SnapshotSeq
}

func shouldAttemptSnapshotRefresh(targetSeq, lastAttemptSeq int64, lastAttemptAt, now time.Time) bool {
	if targetSeq != lastAttemptSeq {
		return true
	}
	if lastAttemptAt.IsZero() {
		return true
	}
	return now.Sub(lastAttemptAt) >= snapshotRefreshRetryInterval
}

func (a *App) persistSortSpec() {
	a.cfg.SortColumn = string(a.sortSpec.Column)
	a.cfg.SortDirection = string(a.sortSpec.Direction)
	cfgPath, err := config.ConfigPath()
	if err != nil {
		a.logger.Printf("resolve config path failed while saving sort: %v", err)
		return
	}
	if err := config.Save(cfgPath, a.cfg); err != nil {
		a.logger.Printf("save config failed while saving sort: %v", err)
	}
}
