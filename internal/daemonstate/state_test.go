package daemonstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResumeStateLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon", "reindex.resume.json")

	if _, err := ReadResumeState(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected not exist before write, got %v", err)
	}

	if err := WriteResumeState(path, ReindexResumeState{ScanID: 42, Root: "/root", CurrentPath: "/root/a.txt"}); err != nil {
		t.Fatal(err)
	}

	st, err := ReadResumeState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.ScanID != 42 || st.Root != "/root" || st.CurrentPath != "/root/a.txt" {
		t.Fatalf("unexpected resume state %+v", st)
	}
	if st.UpdatedAt.IsZero() {
		t.Fatal("expected UpdatedAt to be set")
	}

	if err := ClearResumeState(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected state to be removed, got %v", err)
	}

	if err := ClearResumeState(path); err != nil {
		t.Fatal(err)
	}
}

func TestReindexTriggerLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon", "reindex.trigger")

	if err := TriggerReindex(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected trigger file to exist, got %v", err)
	}

	triggered, err := ConsumeTrigger(path)
	if err != nil {
		t.Fatal(err)
	}
	if !triggered {
		t.Fatal("expected trigger to be consumed")
	}

	triggered, err = ConsumeTrigger(path)
	if err != nil {
		t.Fatal(err)
	}
	if triggered {
		t.Fatal("expected no trigger after consume")
	}
}

func TestStopAndFreshSignalLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "daemon")
	stopPath := filepath.Join(dir, "reindex.stop")
	freshPath := filepath.Join(dir, "reindex.fresh")

	if err := RequestReindexStop(stopPath); err != nil {
		t.Fatal(err)
	}
	if err := RequestFreshReindex(freshPath); err != nil {
		t.Fatal(err)
	}

	stopRequested, err := ConsumeReindexStop(stopPath)
	if err != nil {
		t.Fatal(err)
	}
	if !stopRequested {
		t.Fatal("expected stop request to be consumed")
	}

	freshRequested, err := ConsumeFreshReindex(freshPath)
	if err != nil {
		t.Fatal(err)
	}
	if !freshRequested {
		t.Fatal("expected fresh request to be consumed")
	}

	if err := RequestReindexStop(stopPath); err != nil {
		t.Fatal(err)
	}
	if err := ClearReindexStop(stopPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stopPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stop signal to be removed, got %v", err)
	}

	if err := RequestFreshReindex(freshPath); err != nil {
		t.Fatal(err)
	}
	if err := ClearFreshReindex(freshPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(freshPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected fresh signal to be removed, got %v", err)
	}
}
