package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	IncludePaths      []string `json:"include_paths"`
	ExcludeGlobs      []string `json:"exclude_globs"`
	IndexDBPath       string   `json:"index_db_path"`
	StoreBackend      string   `json:"store_backend"`
	MaxResults        int      `json:"max_results"`
	DebounceMs        int      `json:"debounce_ms"`
	ScanBatchSize     int      `json:"scan_batch_size"`
	ScanThrottleEvery int      `json:"scan_throttle_every"`
	ScanThrottleMs    int      `json:"scan_throttle_ms"`
	DaemonDir         string   `json:"daemon_dir"`
	SortColumn        string   `json:"sort_column"`
	SortDirection     string   `json:"sort_direction"`
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "omnia-search"), nil
}

func Default() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}

	include := []string{home}
	if _, err := os.Stat("/Volumes"); err == nil {
		include = append(include, "/Volumes")
	}
	return Config{
		IncludePaths: include,
		ExcludeGlobs: []string{
			".git",
			"node_modules",
			"Library/Caches",
			".Trash",
			"Trash",
		},
		IndexDBPath:       "index.bleve",
		StoreBackend:      "bleve",
		MaxResults:        5000,
		DebounceMs:        120,
		ScanBatchSize:     1000,
		ScanThrottleEvery: 250,
		ScanThrottleMs:    5,
		DaemonDir:         "daemon",
		SortColumn:        "name",
		SortDirection:     "ASC",
	}, nil
}

func ConfigPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func Load() (Config, error) {
	defaults, err := Default()
	if err != nil {
		return Config{}, err
	}
	cfgPath, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return Config{}, err
	}

	if _, statErr := os.Stat(cfgPath); errors.Is(statErr, os.ErrNotExist) {
		if err := Save(cfgPath, defaults); err != nil {
			return Config{}, err
		}
		return resolveRuntimePaths(normalize(defaults)), nil
	}

	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	cfg := defaults
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return resolveRuntimePaths(normalize(cfg)), nil
}

func Save(path string, cfg Config) error {
	cfg = normalize(cfg)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func normalize(cfg Config) Config {
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = 5000
	}
	if cfg.DebounceMs <= 0 {
		cfg.DebounceMs = 120
	}
	if cfg.ScanBatchSize <= 0 {
		cfg.ScanBatchSize = 1000
	}
	if cfg.ScanThrottleEvery <= 0 {
		cfg.ScanThrottleEvery = 250
	}
	if cfg.ScanThrottleMs < 0 {
		cfg.ScanThrottleMs = 0
	}
	if cfg.DaemonDir == "" {
		if d, err := Default(); err == nil {
			cfg.DaemonDir = d.DaemonDir
		}
	}
	if len(cfg.IncludePaths) == 0 {
		if d, err := Default(); err == nil {
			cfg.IncludePaths = d.IncludePaths
		}
	}
	seen := make(map[string]struct{}, len(cfg.IncludePaths)+1)
	cleaned := make([]string, 0, len(cfg.IncludePaths)+1)
	for _, p := range cfg.IncludePaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		cleaned = append(cleaned, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		home = filepath.Clean(home)
		if _, ok := seen[home]; !ok {
			cleaned = append([]string{home}, cleaned...)
		}
	}
	cfg.IncludePaths = cleaned
	if cfg.IndexDBPath != "" {
		cfg.IndexDBPath = filepath.Clean(cfg.IndexDBPath)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.StoreBackend)) {
	case "", "bleve":
		cfg.StoreBackend = "bleve"
	default:
		cfg.StoreBackend = strings.ToLower(strings.TrimSpace(cfg.StoreBackend))
	}
	if cfg.StoreBackend == "bleve" {
		cfg.IndexDBPath = coerceBleveIndexPath(cfg.IndexDBPath)
	}
	if cfg.DaemonDir != "" {
		cfg.DaemonDir = filepath.Clean(cfg.DaemonDir)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.SortColumn)) {
	case "name", "path", "size", "created", "modified":
		cfg.SortColumn = strings.ToLower(strings.TrimSpace(cfg.SortColumn))
	default:
		cfg.SortColumn = "name"
	}
	switch strings.ToUpper(strings.TrimSpace(cfg.SortDirection)) {
	case "ASC", "DESC":
		cfg.SortDirection = strings.ToUpper(strings.TrimSpace(cfg.SortDirection))
	default:
		cfg.SortDirection = "ASC"
	}
	return cfg
}

func resolveRuntimePaths(cfg Config) Config {
	if cfgDir, err := configDir(); err == nil {
		if cfg.IndexDBPath != "" && !filepath.IsAbs(cfg.IndexDBPath) {
			cfg.IndexDBPath = filepath.Join(cfgDir, cfg.IndexDBPath)
		}
		if cfg.DaemonDir != "" && !filepath.IsAbs(cfg.DaemonDir) {
			cfg.DaemonDir = filepath.Join(cfgDir, cfg.DaemonDir)
		}
	}
	if cfg.IndexDBPath != "" {
		cfg.IndexDBPath = filepath.Clean(cfg.IndexDBPath)
	}
	if cfg.DaemonDir != "" {
		cfg.DaemonDir = filepath.Clean(cfg.DaemonDir)
	}
	return cfg
}

func coerceBleveIndexPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "index.bleve"
	}
	clean := filepath.Clean(path)
	if strings.EqualFold(filepath.Ext(clean), ".db") {
		dir := filepath.Dir(clean)
		if dir == "." {
			return "index.bleve"
		}
		return filepath.Join(dir, "index.bleve")
	}
	return clean
}

func (c Config) DaemonStatusPath() string {
	return filepath.Join(c.DaemonDir, "status.json")
}

func (c Config) DaemonTriggerPath() string {
	return filepath.Join(c.DaemonDir, "reindex.trigger")
}

func (c Config) DaemonStopPath() string {
	return filepath.Join(c.DaemonDir, "reindex.stop")
}

func (c Config) DaemonFreshStartPath() string {
	return filepath.Join(c.DaemonDir, "reindex.fresh")
}

func (c Config) DaemonResumeStatePath() string {
	return filepath.Join(c.DaemonDir, "reindex.resume.json")
}
