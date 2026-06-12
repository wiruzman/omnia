package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeDefaults(t *testing.T) {
	cfg := normalize(Config{MaxResults: 0, DebounceMs: 0, ScanBatchSize: 0})
	if cfg.MaxResults <= 0 || cfg.DebounceMs <= 0 || cfg.ScanBatchSize <= 0 {
		t.Fatal("expected defaults to be set")
	}
}

func TestNormalizeInjectsHomeIncludePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := normalize(Config{IncludePaths: []string{"/Volumes"}})
	if len(cfg.IncludePaths) == 0 {
		t.Fatal("expected include paths")
	}
	if cfg.IncludePaths[0] != filepath.Clean(home) {
		t.Fatalf("expected first include path to be home dir, got %q", cfg.IncludePaths[0])
	}
}

func TestLoadResolvesRelativeRuntimePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgPath, err := ConfigPath()
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	err = Save(cfgPath, Config{
		IncludePaths:  []string{"/Volumes"},
		IndexDBPath:   "index.bleve",
		StoreBackend:  "bleve",
		MaxResults:    10,
		DebounceMs:    10,
		ScanBatchSize: 10,
		DaemonDir:     "daemon",
		SortColumn:    "name",
		SortDirection: "ASC",
	})
	if err != nil {
		t.Fatalf("save config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	configBase := filepath.Join(home, ".config", "omnia-search")
	if cfg.IndexDBPath != filepath.Join(configBase, "index.bleve") {
		t.Fatalf("unexpected index_db_path: %q", cfg.IndexDBPath)
	}
	if cfg.DaemonDir != filepath.Join(configBase, "daemon") {
		t.Fatalf("unexpected daemon_dir: %q", cfg.DaemonDir)
	}
	if len(cfg.IncludePaths) == 0 || cfg.IncludePaths[0] != filepath.Clean(home) {
		t.Fatalf("expected home include path to be injected, got %v", cfg.IncludePaths)
	}
}

func TestNormalizeCoercesBleveDBPath(t *testing.T) {
	cfg := normalize(Config{StoreBackend: "bleve", IndexDBPath: "custom.db"})
	if cfg.IndexDBPath != filepath.Join(".", "index.bleve") && cfg.IndexDBPath != "index.bleve" {
		t.Fatalf("expected .db path to be coerced to index.bleve, got %q", cfg.IndexDBPath)
	}

	cfg = normalize(Config{StoreBackend: "bleve", IndexDBPath: "/tmp/custom.db"})
	if cfg.IndexDBPath != "/tmp/index.bleve" {
		t.Fatalf("expected absolute .db path to be coerced, got %q", cfg.IndexDBPath)
	}
}
