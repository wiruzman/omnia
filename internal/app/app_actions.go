package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"omnia-search-tui/internal/model"
)

func (a *App) openSelected() {
	e := a.currentEntry()
	if e == nil {
		return
	}
	if err := a.system.OpenPath(e.Path); err != nil {
		a.logger.Printf("open path failed: %v", err)
	}
}

func (a *App) revealSelected() {
	e := a.currentEntry()
	if e == nil {
		return
	}
	if err := a.system.RevealInFinder(e.Path); err != nil {
		a.logger.Printf("reveal failed: %v", err)
	}
}

func (a *App) copySelectedPath() {
	e := a.currentEntry()
	if e == nil {
		return
	}
	if err := a.system.CopyToClipboard(e.Path); err != nil {
		a.logger.Printf("copy failed: %v", err)
	}
}

func (a *App) confirmDeleteSelected() {
	e := a.currentEntry()
	if e == nil {
		return
	}

	path := e.Path
	modal := tview.NewModal().
		SetText(fmt.Sprintf("Move this item to Trash?\n\n%s", trimMiddle(path, 120))).
		SetTextColor(tcell.ColorDefault).
		SetButtonStyle(tcell.StyleDefault).
		SetButtonActivatedStyle(tcell.StyleDefault.Bold(true).Underline(true)).
		AddButtons([]string{"Cancel", "Delete"}).
		SetDoneFunc(func(_ int, buttonLabel string) {
			a.pages.RemovePage("confirm-delete")
			a.tui.SetFocus(a.table)
			if buttonLabel != "Delete" {
				return
			}
			a.deletePathAsync(path)
		})
	modal.SetBackgroundColor(tcell.ColorDefault)
	a.pages.AddPage("confirm-delete", modal, true, true)
	a.tui.SetFocus(modal)
}

func (a *App) deletePath(path string) {
	a.invalidatePendingRefreshes()

	if err := a.system.MoveToTrash(path); err != nil {
		a.logger.Printf("delete failed for %s: %v", path, err)
		return
	}
	a.forgetEmptyQueryResults()

	if err := a.deleteFromIndexWithRetry(context.Background(), path); err != nil {
		a.logger.Printf("index delete failed for %s: %v", path, err)
		a.removeFromCurrentEntries(path)
		a.invalidatePendingRefreshes()
		go a.reconcileDeleteAsync(path)
		return
	}
	a.removeFromCurrentEntries(path)
	a.invalidatePendingRefreshes()
	a.requestRefreshAsync(a.query, a.sortSpec)
}

func (a *App) deletePathAsync(path string) {
	a.setDeleteInProgress(path)
	a.updateStatus()

	go func() {
		a.invalidatePendingRefreshes()

		if err := a.system.MoveToTrash(path); err != nil {
			a.logger.Printf("delete failed for %s: %v", path, err)
			a.tui.QueueUpdateDraw(func() {
				a.clearDeleteInProgress(path)
				a.updateStatus()
			})
			return
		}
		a.forgetEmptyQueryResults()

		// Remove from visible results and stop delete progress as soon as the file is in Trash.
		a.tui.QueueUpdateDraw(func() {
			a.removeFromCurrentEntries(path)
			a.clearDeleteInProgress(path)
			a.updateStatus()
		})

		if err := a.deleteFromIndexWithRetry(context.Background(), path); err != nil {
			a.logger.Printf("index delete failed for %s: %v", path, err)
			a.invalidatePendingRefreshes()
			go a.reconcileDeleteAsync(path)
			return
		}

		a.invalidatePendingRefreshes()
		a.tui.QueueUpdateDraw(func() {
			a.requestRefreshAsync(a.query, a.sortSpec)
		})
	}()
}

func (a *App) setDeleteInProgress(path string) {
	a.deleteMu.Lock()
	a.deletingPath = path
	a.deleteMu.Unlock()
	a.deleteState.Store(1)
}

func (a *App) clearDeleteInProgress(path string) {
	a.deleteMu.Lock()
	if a.deletingPath == path {
		a.deletingPath = ""
	}
	a.deleteMu.Unlock()
	a.deleteState.Store(0)
}

func (a *App) deleteProgress() (bool, string) {
	if a.deleteState.Load() == 0 {
		return false, ""
	}
	a.deleteMu.Lock()
	path := a.deletingPath
	a.deleteMu.Unlock()
	return true, path
}

func (a *App) currentEntry() *model.Entry {
	if a.selected < 0 || a.selected >= len(a.entries) {
		return nil
	}
	return &a.entries[a.selected]
}

func (a *App) toggleDetails() {
	e := a.currentEntry()
	if e == nil {
		return
	}
	text := fmt.Sprintf("Path: %s\nName: %s\nType: %s\nSize: %d\nCreated: %s\nModified: %s",
		e.Path, e.Name, e.Type, e.Size, e.CreatedAt.Format(time.RFC3339), e.ModifiedAt.Format(time.RFC3339))
	a.details.SetText(text)
	if name, _ := a.pages.GetFrontPage(); name == "details" {
		a.pages.HidePage("details")
		a.tui.SetFocus(a.table)
		return
	}
	a.pages.ShowPage("details")
	a.tui.SetFocus(a.details)
}

func (a *App) deleteFromIndexWithRetry(ctx context.Context, path string) error {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		err := a.storeDeletePathPrefix(ctx, path)
		if err == nil {
			return nil
		}
		lastErr = err
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "locked") && !strings.Contains(msg, "busy") {
			return err
		}
		time.Sleep(time.Duration(60*(attempt+1)) * time.Millisecond)
	}
	return lastErr
}

func (a *App) removeFromCurrentEntries(path string) {
	path = filepath.Clean(path)
	prefix := path + string(os.PathSeparator)
	filtered := make([]model.Entry, 0, len(a.entries))
	for _, e := range a.entries {
		p := filepath.Clean(e.Path)
		if p == path || strings.HasPrefix(p, prefix) {
			continue
		}
		filtered = append(filtered, e)
	}
	a.applyResults(filtered, len(filtered))
}

func (a *App) reconcileDeleteAsync(path string) {
	for attempt := 0; attempt < 4; attempt++ {
		time.Sleep(time.Duration(400*(attempt+1)) * time.Millisecond)
		if err := a.deleteFromIndexWithRetry(context.Background(), path); err != nil {
			a.logger.Printf("async index delete retry failed for %s: %v", path, err)
			continue
		}
		a.invalidatePendingRefreshes()
		a.tui.QueueUpdateDraw(func() {
			a.refreshData(context.Background())
		})
		return
	}
}
