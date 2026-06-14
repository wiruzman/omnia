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

func TestDefaultScanThrottle(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	if cfg.IndexDBPath != "index.sqlite" {
		t.Fatalf("expected default index_db_path index.sqlite, got %q", cfg.IndexDBPath)
	}
	if cfg.ScanThrottleEvery != 250 {
		t.Fatalf("expected default scan_throttle_every 250, got %d", cfg.ScanThrottleEvery)
	}
	if cfg.ScanThrottleMs != 5 {
		t.Fatalf("expected default scan_throttle_ms 5, got %d", cfg.ScanThrottleMs)
	}
	if cfg.DaemonLogFile != "daemon.log" {
		t.Fatalf("expected default daemon_log_file daemon.log, got %q", cfg.DaemonLogFile)
	}
	if cfg.DaemonLogLevel != "info" {
		t.Fatalf("expected default daemon_log_level info, got %q", cfg.DaemonLogLevel)
	}
	if cfg.DaemonLogMaxBytes != 10*1024*1024 {
		t.Fatalf("expected default daemon_log_max_bytes 10MiB, got %d", cfg.DaemonLogMaxBytes)
	}
	if cfg.DaemonLogBackups != 5 {
		t.Fatalf("expected default daemon_log_backups 5, got %d", cfg.DaemonLogBackups)
	}
}

func TestNormalizeAllowsDisablingScanThrottle(t *testing.T) {
	cfg := normalize(Config{ScanThrottleEvery: 0, ScanThrottleMs: 0})
	if cfg.ScanThrottleEvery != 250 {
		t.Fatalf("expected scan_throttle_every to fall back to 250, got %d", cfg.ScanThrottleEvery)
	}
	if cfg.ScanThrottleMs != 0 {
		t.Fatalf("expected scan_throttle_ms 0 to disable throttling, got %d", cfg.ScanThrottleMs)
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
		IndexDBPath:   "index.sqlite",
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
	if cfg.IndexDBPath != filepath.Join(configBase, "index.sqlite") {
		t.Fatalf("unexpected index_db_path: %q", cfg.IndexDBPath)
	}
	if cfg.DaemonDir != filepath.Join(configBase, "daemon") {
		t.Fatalf("unexpected daemon_dir: %q", cfg.DaemonDir)
	}
	if cfg.DaemonLogFile != filepath.Join(configBase, "daemon", "daemon.log") {
		t.Fatalf("unexpected daemon_log_file: %q", cfg.DaemonLogFile)
	}
	if len(cfg.IncludePaths) == 0 || cfg.IncludePaths[0] != filepath.Clean(home) {
		t.Fatalf("expected home include path to be injected, got %v", cfg.IncludePaths)
	}
}

func TestNormalizeDaemonLogOptions(t *testing.T) {
	cfg := normalize(Config{
		DaemonLogLevel:    "warning",
		DaemonLogMaxBytes: -1,
		DaemonLogBackups:  -1,
	})

	if cfg.DaemonLogLevel != "warn" {
		t.Fatalf("expected warning to normalize to warn, got %q", cfg.DaemonLogLevel)
	}
	if cfg.DaemonLogMaxBytes != 10*1024*1024 {
		t.Fatalf("expected daemon_log_max_bytes default, got %d", cfg.DaemonLogMaxBytes)
	}
	if cfg.DaemonLogBackups != 5 {
		t.Fatalf("expected daemon_log_backups default, got %d", cfg.DaemonLogBackups)
	}
}

func TestNormalizeDefaultsIndexPath(t *testing.T) {
	cfg := normalize(Config{})
	if cfg.IndexDBPath != filepath.Join(".", "index.sqlite") && cfg.IndexDBPath != "index.sqlite" {
		t.Fatalf("expected default index path, got %q", cfg.IndexDBPath)
	}
}
