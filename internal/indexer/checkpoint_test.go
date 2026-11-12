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
	"github.com/wiruzman/omnia/internal/store"
)

func checkpointFixture(t *testing.T, files, batchSize int) (config.Config, *store.SQLiteStore) {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < files; i++ {
		writeSizedFile(t, filepath.Join(root, fmt.Sprintf("file-%04d.dat", i)), 1)
	}
	cfg := config.Config{IncludePaths: []string{root}, IndexDBPath: filepath.Join(t.TempDir(), "index.sqlite"), DaemonDir: t.TempDir(), ScanBatchSize: batchSize, ScanThrottleEvery: 250, ScanThrottleMs: 100, MaxResults: 20, SortColumn: "name", SortDirection: "ASC"}
	st, err := store.OpenSQLite(cfg.IndexDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var entries []model.Entry
	if err := scanner.New(nil).Walk(cfg.IncludePaths, func(e model.Entry) error { entries = append(entries, e); return nil }, nil, func(error) {}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBatch(context.Background(), 1, entries); err != nil {
		t.Fatal(err)
	}
	return cfg, st
}

func resumeAndCheckCount(t *testing.T, cfg config.Config, st store.Backend, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	idx := New(cfg, scanner.New(nil), st, log.New(io.Discard, "", 0))
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	if s := idx.CurrentStatus(); s.LastError != "" || s.Running {
		t.Fatalf("resume failed: %+v", s)
	}
	if n, err := st.Count(ctx); err != nil || n != want {
		t.Fatalf("resume lost cached entries: count=%d want=%d err=%v", n, want, err)
	}
}

func TestResumeNeverSkipsUncommittedEntries(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		files, batch, stopAt int
	}{
		{"before_first_commit", 700, 1000, 250},
		{"partial_second_batch", 1000, 300, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, st := checkpointFixture(t, tc.files, tc.batch)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			idx := New(cfg, scanner.New(nil), st, log.New(io.Discard, "", 0))
			if err := idx.StartReindex(ctx); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for idx.CurrentStatus().Scanned < int64(tc.stopAt) {
				if time.Now().After(deadline) {
					cancel()
					idx.Wait()
					t.Fatal("no scan progress")
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			idx.Wait()
			resumeAndCheckCount(t, cfg, st, tc.files+1)
		})
	}
}

type checkpointStore struct {
	store.Backend
	calls, failAt int
	lastCommitted model.Entry
	afterCommit   func() error
	cleanupCalled bool
	cancelOnCount context.CancelFunc
}

func (s *checkpointStore) CountByRoots(ctx context.Context, roots []string) (map[string]int64, error) {
	if s.cancelOnCount != nil {
		s.cancelOnCount()
		return nil, ctx.Err()
	}
	return s.Backend.CountByRoots(ctx, roots)
}

func (s *checkpointStore) UpsertBatch(ctx context.Context, id int64, batch []model.Entry) error {
	s.calls++
	if s.calls == s.failAt {
		return errors.New("fixture batch failure")
	}
	if err := s.Backend.UpsertBatch(ctx, id, batch); err != nil {
		return err
	}
	s.lastCommitted = batch[len(batch)-1]
	if s.afterCommit != nil {
		return s.afterCommit()
	}
	return nil
}

func (s *checkpointStore) CleanupStale(ctx context.Context, id int64, roots []string) error {
	s.cleanupCalled = true
	return s.Backend.CleanupStale(ctx, id, roots)
}

func TestFailedBatchDoesNotAdvanceCheckpoint(t *testing.T) {
	cfg, st := checkpointFixture(t, 1000, 300)
	wrapper := &checkpointStore{Backend: st, failAt: 2}
	idx := New(cfg, scanner.New(nil), wrapper, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	checkpoint, err := daemonstate.ReadResumeState(cfg.DaemonResumeStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.CurrentPath != wrapper.lastCommitted.Path || checkpoint.Root != wrapper.lastCommitted.RootPath {
		t.Fatalf("checkpoint outran commit: checkpoint=%+v committed=%s", checkpoint, wrapper.lastCommitted.Path)
	}
	if !checkpoint.Committed {
		t.Fatal("successful commit did not qualify the checkpoint")
	}
	if wrapper.cleanupCalled || idx.CurrentStatus().LastError == "" {
		t.Fatal("failed batch did not abort before cleanup")
	}
	resumeAndCheckCount(t, cfg, st, 1001)
}

func TestCheckpointFailureAbortsBeforeCleanup(t *testing.T) {
	cfg, st := checkpointFixture(t, 400, 300)
	wrapper := &checkpointStore{Backend: st, afterCommit: func() error { return os.Mkdir(cfg.DaemonResumeStatePath()+".tmp", 0700) }}
	idx := New(cfg, scanner.New(nil), wrapper, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	if wrapper.calls != 1 || wrapper.cleanupCalled || idx.CurrentStatus().LastError == "" {
		t.Fatalf("unsafe checkpoint failure: calls=%d cleanup=%t status=%+v", wrapper.calls, wrapper.cleanupCalled, idx.CurrentStatus())
	}
}

func TestInitialCheckpointFailureDoesNotStartWalking(t *testing.T) {
	cfg, st := checkpointFixture(t, 2, 1000)
	if err := os.Mkdir(cfg.DaemonResumeStatePath()+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := &checkpointStore{Backend: st}
	idx := New(cfg, scanner.New(nil), wrapper, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	if wrapper.calls != 0 || wrapper.cleanupCalled || idx.CurrentStatus().LastError == "" {
		t.Fatal("initial checkpoint failure allowed scan writes/cleanup")
	}
}

func TestLegacyCheckpointRestartsWithFreshScanID(t *testing.T) {
	cfg, st := checkpointFixture(t, 700, 1000)
	cfg.ScanThrottleMs = 0
	// A legacy cursor may point beyond uncommitted entries. Also seed an old
	// marker on a now-missing entry: reusing that scan ID would retain it.
	ghost := model.Entry{Path: filepath.Join(cfg.IncludePaths[0], "missing.dat"), Name: "missing.dat", RootPath: cfg.IncludePaths[0], Type: model.TypeFile}
	if err := st.UpsertBatch(context.Background(), 2, []model.Entry{ghost}); err != nil {
		t.Fatal(err)
	}
	if err := daemonstate.WriteResumeState(cfg.DaemonResumeStatePath(), daemonstate.ReindexResumeState{ScanID: 2, Root: cfg.IncludePaths[0], CurrentPath: filepath.Join(cfg.IncludePaths[0], "file-0248.dat")}); err != nil {
		t.Fatal(err)
	}
	resumeAndCheckCount(t, cfg, st, 701)
}

func TestTrustedCheckpointSurvivesEarlyResumeInterruption(t *testing.T) {
	cfg, st := checkpointFixture(t, 20, 1000)
	want := daemonstate.ReindexResumeState{ScanID: 42, Root: cfg.IncludePaths[0], CurrentPath: filepath.Join(cfg.IncludePaths[0], "file-0009.dat"), Committed: true}
	if err := daemonstate.WriteResumeState(cfg.DaemonResumeStatePath(), want); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapper := &checkpointStore{Backend: st, cancelOnCount: cancel}
	idx := New(cfg, scanner.New(nil), wrapper, log.New(io.Discard, "", 0))
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	got, err := daemonstate.ReadResumeState(cfg.DaemonResumeStatePath())
	if err != nil || got.ScanID != want.ScanID || got.Root != want.Root || got.CurrentPath != want.CurrentPath || !got.Committed {
		t.Fatalf("trusted checkpoint discarded: %+v err=%v", got, err)
	}
}

func TestPartialCrossRootBatchCheckpointsCommittedTail(t *testing.T) {
	cfg, st := checkpointFixture(t, 2, 1000)
	second := t.TempDir()
	for i := 0; i < 2; i++ {
		writeSizedFile(t, filepath.Join(second, fmt.Sprintf("other-%d.dat", i)), 1)
	}
	cfg.IncludePaths = append(cfg.IncludePaths, second)
	var entries []model.Entry
	if err := scanner.New(nil).Walk([]string{second}, func(e model.Entry) error { entries = append(entries, e); return nil }, nil, func(error) {}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBatch(context.Background(), 1, entries); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapper := &checkpointStore{Backend: st, afterCommit: func() error { cancel(); return nil }}
	idx := New(cfg, scanner.New(nil), wrapper, log.New(io.Discard, "", 0))
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	idx.Wait()
	checkpoint, err := daemonstate.ReadResumeState(cfg.DaemonResumeStatePath())
	if err != nil || wrapper.calls != 1 || checkpoint.Root != second || checkpoint.CurrentPath != wrapper.lastCommitted.Path || !checkpoint.Committed {
		t.Fatalf("bad partial/cross-root checkpoint: %+v calls=%d err=%v", checkpoint, wrapper.calls, err)
	}
	resumeAndCheckCount(t, cfg, st, 6)
}

func TestResumePreservesLaterSiblings(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "ordinary"
		if blocked {
			name = "inaccessible"
		}
		t.Run(name, func(t *testing.T) {
			cfg, st := checkpointFixture(t, 0, 3)
			root := cfg.IncludePaths[0]
			for _, name := range []string{"a/file", "a-b", "a.txt", "a-blocked/preserveneedle.txt"} {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				writeSizedFile(t, path, 1)
			}
			var entries []model.Entry
			if err := scanner.New(nil).Walk(cfg.IncludePaths, func(e model.Entry) error {
				entries = append(entries, e)
				return nil
			}, nil, func(error) {}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertBatch(context.Background(), 1, entries); err != nil {
				t.Fatal(err)
			}
			if blocked {
				dir := filepath.Join(root, "a-blocked")
				if err := os.Chmod(dir, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(dir, 0700) })
				if _, err := os.ReadDir(dir); !os.IsPermission(err) {
					t.Skip("platform can enumerate inaccessible fixture")
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wrapper := &checkpointStore{Backend: st, afterCommit: func() error { cancel(); return nil }}
			idx := New(cfg, scanner.New(nil), wrapper, log.New(io.Discard, "", 0))
			if err := idx.StartReindex(ctx); err != nil {
				t.Fatal(err)
			}
			idx.Wait()
			checkpoint, err := daemonstate.ReadResumeState(cfg.DaemonResumeStatePath())
			if err != nil || !checkpoint.Committed || checkpoint.CurrentPath != filepath.Join(root, "a", "file") {
				t.Fatalf("unexpected checkpoint: %+v err=%v", checkpoint, err)
			}
			resumeAndCheckCount(t, cfg, st, len(entries))
			if !hasEntry(t, st, "preserveneedle") {
				t.Fatal("resume discarded entry under later sibling")
			}
		})
	}
}
