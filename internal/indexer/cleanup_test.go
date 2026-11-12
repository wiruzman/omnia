package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/daemonstate"
	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/scanner"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/store"
)

func unreadableFixture(t *testing.T, extra int) (config.Config, *store.SQLiteStore, string, string) {
	t.Helper()
	root := t.TempDir()
	blocked := filepath.Join(root, "a-blocked")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(blocked, "preserveneedle.txt")
	writeSizedFile(t, child, 10)
	for i := 0; i < extra; i++ {
		writeSizedFile(t, filepath.Join(root, fmt.Sprintf("z-%04d.txt", i)), 1)
	}
	cfg := config.Config{IncludePaths: []string{root}, IndexDBPath: filepath.Join(t.TempDir(), "index.sqlite"), DaemonDir: t.TempDir(), ScanBatchSize: 50, MaxResults: 20, SortColumn: "name", SortDirection: "ASC"}
	st, err := store.OpenSQLite(cfg.IndexDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var entries []model.Entry
	if err := scanner.New(nil).Walk([]string{root}, func(e model.Entry) error { entries = append(entries, e); return nil }, nil, func(error) {}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBatch(context.Background(), 1, entries); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0700) })
	if _, err := os.ReadDir(blocked); !os.IsPermission(err) {
		t.Skip("platform can enumerate inaccessible fixture")
	}
	return cfg, st, blocked, child
}

func hasEntry(t *testing.T, st store.Backend, query string) bool {
	t.Helper()
	r, err := st.Query(context.Background(), query, sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(r.Entries) > 0
}

func TestReindexPreservesUnreadableButCleansMissingAndExcluded(t *testing.T) {
	cfg, st, _, child := unreadableFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Seed a missing path and an intentionally excluded subtree. Neither is
	// an access failure, so those old entries should still be cleaned up.
	for _, name := range []string{"missingneedle.txt", "excluded/dropneedle.txt"} {
		e := model.Entry{Path: filepath.Join(cfg.IncludePaths[0], name), Name: filepath.Base(name), RootPath: cfg.IncludePaths[0], Type: model.TypeFile}
		if err := st.UpsertBatch(ctx, 1, []model.Entry{e}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(cfg.IncludePaths[0], "excluded"), 0700); err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, filepath.Join(cfg.IncludePaths[0], "excluded", "dropneedle.txt"), 1)
	idx := New(cfg, scanner.New([]string{"excluded"}), st, log.New(io.Discard, "", 0))
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	if s := idx.CurrentStatus(); s.LastError != "" {
		t.Fatal(s.LastError)
	}
	if !hasEntry(t, st, "preserveneedle") || hasEntry(t, st, "missingneedle") || hasEntry(t, st, "dropneedle") {
		t.Fatal("unreadable/missing/excluded cleanup semantics regressed")
	}
	if err := os.Chmod(filepath.Dir(child), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(child); err != nil {
		t.Fatal(err)
	}
}

type interruptProtectedScan struct {
	store.Backend
	cancel             context.CancelFunc
	protected, batches int
}

func (s *interruptProtectedScan) MarkSubtreeSeen(ctx context.Context, id int64, path string) error {
	if err := s.Backend.MarkSubtreeSeen(ctx, id, path); err != nil {
		return err
	}
	s.protected++
	return nil
}
func (s *interruptProtectedScan) UpsertBatch(ctx context.Context, id int64, batch []model.Entry) error {
	if err := s.Backend.UpsertBatch(ctx, id, batch); err != nil {
		return err
	}
	s.batches++
	if s.batches == 6 {
		s.cancel()
	}
	return nil
}

func TestUnreadableProtectionSurvivesInterruptedScanResume(t *testing.T) {
	cfg, st, blocked, _ := unreadableFixture(t, 400)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapper := &interruptProtectedScan{Backend: st, cancel: cancel}
	idx := New(cfg, scanner.New(nil), wrapper, log.New(io.Discard, "", 0))
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	resume, err := daemonstate.ReadResumeState(cfg.DaemonResumeStatePath())
	if err != nil || resume.CurrentPath <= blocked || wrapper.protected != 1 {
		t.Fatalf("bad interrupted checkpoint: %+v protection=%d err=%v", resume, wrapper.protected, err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	idx2 := New(cfg, scanner.New(nil), st, log.New(io.Discard, "", 0))
	if err := idx2.StartReindex(ctx2); err != nil {
		t.Fatal(err)
	}
	idx2.Wait()
	if s := idx2.CurrentStatus(); s.LastError != "" || s.Running {
		t.Fatalf("resume failed: %+v", s)
	}
	if !hasEntry(t, st, "preserveneedle") {
		t.Fatal("resume discarded previously protected subtree")
	}
}

type rejectProtection struct {
	store.Backend
	cleanupCalled bool
}

func (s *rejectProtection) MarkSubtreeSeen(context.Context, int64, string) error {
	return errors.New("fixture preservation failure")
}
func (s *rejectProtection) CleanupStale(ctx context.Context, id int64, roots []string) error {
	s.cleanupCalled = true
	return s.Backend.CleanupStale(ctx, id, roots)
}

func TestFailedProtectionAbortsBeforeCleanup(t *testing.T) {
	cfg, st, _, _ := unreadableFixture(t, 0)
	wrapper := &rejectProtection{Backend: st}
	idx := New(cfg, scanner.New(nil), wrapper, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	if idx.CurrentStatus().LastError == "" || wrapper.cleanupCalled || !hasEntry(t, st, "preserveneedle") {
		t.Fatal("failed preservation allowed destructive cleanup")
	}
}

func TestUnreadableProtectionResumesAtDirectoryCheckpoint(t *testing.T) {
	cfg, st, blocked, _ := unreadableFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries := make([]model.Entry, 400)
	for i := range entries {
		path := filepath.Join(blocked, fmt.Sprintf("cached-%04d.txt", i))
		entries[i] = model.Entry{Path: path, Name: filepath.Base(path), ParentPath: blocked, RootPath: cfg.IncludePaths[0], Type: model.TypeFile}
	}
	if err := st.UpsertBatch(ctx, 1, entries); err != nil {
		t.Fatal(err)
	}
	// Emulate a checkpoint at the directory, with only some protection saved.
	if err := st.UpsertBatch(ctx, 2, entries[:128]); err != nil {
		t.Fatal(err)
	}
	root, err := scanner.New(nil).EntryFromPath(cfg.IncludePaths[0], cfg.IncludePaths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBatch(ctx, 2, []model.Entry{root}); err != nil {
		t.Fatal(err)
	}
	if err := daemonstate.WriteResumeState(cfg.DaemonResumeStatePath(), daemonstate.ReindexResumeState{ScanID: 2, Root: cfg.IncludePaths[0], CurrentPath: blocked, Committed: true}); err != nil {
		t.Fatal(err)
	}
	idx := New(cfg, scanner.New(nil), st, log.New(io.Discard, "", 0))
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	if s := idx.CurrentStatus(); s.LastError != "" || s.Running {
		t.Fatalf("resume failed: %+v", s)
	}
	if n, err := st.Count(ctx); err != nil || n != len(entries)+3 {
		t.Fatalf("partial protection lost entries: count=%d err=%v", n, err)
	}
}
