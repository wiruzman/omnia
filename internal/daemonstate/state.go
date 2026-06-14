package daemonstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wiruzman/omnia/internal/progress"
)

type Status struct {
	Running      bool                    `json:"running"`
	Indexing     bool                    `json:"indexing"`
	Scanned      int64                   `json:"scanned"`
	CurrentPath  string                  `json:"current_path"`
	PathProgress []progress.PathProgress `json:"path_progress,omitempty"`
	LastScanAt   time.Time               `json:"last_scan_at"`
	LastError    string                  `json:"last_error"`
	UpdatedAt    time.Time               `json:"updated_at"`
	IndexedTotal int                     `json:"indexed_total"`
	SnapshotSeq  int64                   `json:"snapshot_seq"`
}

type ReindexResumeState struct {
	ScanID      int64     `json:"scan_id"`
	Root        string    `json:"root"`
	CurrentPath string    `json:"current_path"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func Read(path string) (Status, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

func Write(path string, st Status) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	st.UpdatedAt = time.Now()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func TriggerReindex(path string) error {
	return writeSignal(path)
}

func ClearReindexTrigger(path string) error {
	return clearSignal(path)
}

func ConsumeTrigger(path string) (bool, error) {
	return consumeSignal(path)
}

func RequestReindexStop(path string) error {
	return writeSignal(path)
}

func ClearReindexStop(path string) error {
	return clearSignal(path)
}

func ConsumeReindexStop(path string) (bool, error) {
	return consumeSignal(path)
}

func RequestFreshReindex(path string) error {
	return writeSignal(path)
}

func ClearFreshReindex(path string) error {
	return clearSignal(path)
}

func ConsumeFreshReindex(path string) (bool, error) {
	return consumeSignal(path)
}

func writeSignal(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)), 0o644)
}

func consumeSignal(path string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

func clearSignal(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return os.Remove(path)
}

func ReadResumeState(path string) (ReindexResumeState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ReindexResumeState{}, err
	}
	var st ReindexResumeState
	if err := json.Unmarshal(b, &st); err != nil {
		return ReindexResumeState{}, err
	}
	return st, nil
}

func WriteResumeState(path string, st ReindexResumeState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	st.UpdatedAt = time.Now()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ClearResumeState(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return os.Remove(path)
}
