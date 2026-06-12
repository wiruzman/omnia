package scanner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"omnia-search-tui/internal/model"
)

type Progress struct {
	Scanned     int64
	Current     string
	Root        string
	RootScanned int64
}

type Scanner struct {
	excludes []string
}

type WalkOptions struct {
	ResumeRoot      string
	ResumeAfterPath string
	ThrottleEvery   int
	ThrottleDelay   time.Duration
	throttleSleep   func(time.Duration)
}

func New(excludes []string) *Scanner {
	return &Scanner{excludes: excludes}
}

func (s *Scanner) ShouldExclude(path string) bool {
	normalized := strings.ToLower(filepath.ToSlash(path))
	for _, raw := range s.excludes {
		ex := strings.ToLower(filepath.ToSlash(strings.TrimSpace(raw)))
		if ex == "" {
			continue
		}
		if strings.Contains(normalized, "/"+ex+"/") || strings.HasSuffix(normalized, "/"+ex) || filepath.Base(normalized) == ex {
			return true
		}
	}
	return false
}

func (s *Scanner) Walk(roots []string, emit func(model.Entry) error, progress func(Progress), onWarn func(error)) error {
	return s.WalkWithOptions(roots, emit, progress, onWarn, WalkOptions{})
}

func (s *Scanner) WalkWithOptions(roots []string, emit func(model.Entry) error, progress func(Progress), onWarn func(error), options WalkOptions) error {
	seen := make(map[string]struct{}, 1024)
	var scanned int64
	scannedByRoot := make(map[string]int64, len(roots))
	resumeRoot := filepath.Clean(strings.TrimSpace(options.ResumeRoot))
	resumeAfter := filepath.Clean(strings.TrimSpace(options.ResumeAfterPath))
	if resumeAfter == "." {
		resumeAfter = ""
	}

	rootExists := false
	for _, root := range roots {
		if filepath.Clean(root) == resumeRoot {
			rootExists = true
			break
		}
	}
	if !rootExists {
		resumeRoot = ""
		resumeAfter = ""
	}
	hasReachedResumeRoot := resumeRoot == ""

	for _, root := range roots {
		root = filepath.Clean(root)
		if !hasReachedResumeRoot {
			if root != resumeRoot {
				continue
			}
			hasReachedResumeRoot = true
		}
		if _, err := os.Stat(root); err != nil {
			onWarn(fmt.Errorf("skip missing root %s: %w", root, err))
			continue
		}
		currentResumeAfter := ""
		if root == resumeRoot {
			currentResumeAfter = resumeAfter
		}

		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrPermission) {
					onWarn(fmt.Errorf("permission denied %s", path))
					return fs.SkipDir
				}
				onWarn(fmt.Errorf("walk error %s: %w", path, err))
				return nil
			}
			if s.ShouldExclude(path) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}

			abs, err := filepath.Abs(path)
			if err != nil {
				onWarn(fmt.Errorf("abs path error %s: %w", path, err))
				return nil
			}
			if currentResumeAfter != "" && abs <= currentResumeAfter {
				if d.IsDir() {
					cursorPrefix := currentResumeAfter + string(os.PathSeparator)
					dirPrefix := abs + string(os.PathSeparator)
					if !strings.HasPrefix(cursorPrefix, dirPrefix) {
						return fs.SkipDir
					}
				}
				return nil
			}

			if d.Type()&os.ModeSymlink != 0 {
				if _, ok := seen[abs]; ok {
					return nil
				}
				seen[abs] = struct{}{}
			}

			info, err := d.Info()
			if err != nil {
				onWarn(fmt.Errorf("stat error %s: %w", abs, err))
				return nil
			}

			entryType := model.TypeFile
			if info.Mode()&os.ModeSymlink != 0 {
				entryType = model.TypeSymlink
			} else if info.IsDir() {
				entryType = model.TypeDirectory
			}

			created := info.ModTime()
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				created = time.Unix(int64(stat.Birthtimespec.Sec), int64(stat.Birthtimespec.Nsec))
			}

			entry := model.Entry{
				Path:       abs,
				Name:       filepath.Base(abs),
				ParentPath: filepath.Dir(abs),
				RootPath:   root,
				Type:       entryType,
				Size:       effectiveSize(info),
				CreatedAt:  created,
				ModifiedAt: info.ModTime(),
			}

			if err := emit(entry); err != nil {
				return err
			}
			scanned++
			scannedByRoot[root]++
			if progress != nil && scanned%250 == 0 {
				progress(Progress{Scanned: scanned, Current: abs, Root: root, RootScanned: scannedByRoot[root]})
			}
			options.throttle(scanned)
			return nil
		})
		if err != nil {
			return err
		}
		if progress != nil {
			progress(Progress{Scanned: scanned, Current: root, Root: root, RootScanned: scannedByRoot[root]})
		}
	}
	if progress != nil {
		progress(Progress{Scanned: scanned})
	}
	return nil
}

func (o WalkOptions) throttle(scanned int64) {
	if o.ThrottleEvery <= 0 || o.ThrottleDelay <= 0 || scanned <= 0 {
		return
	}
	if scanned%int64(o.ThrottleEvery) != 0 {
		return
	}
	sleep := o.throttleSleep
	if sleep == nil {
		sleep = time.Sleep
	}
	sleep(o.ThrottleDelay)
}

func (s *Scanner) EntryFromPath(path string, root string) (model.Entry, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return model.Entry{}, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return model.Entry{}, err
	}

	entryType := model.TypeFile
	if info.Mode()&os.ModeSymlink != 0 {
		entryType = model.TypeSymlink
	} else if info.IsDir() {
		entryType = model.TypeDirectory
	}

	created := info.ModTime()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		created = time.Unix(int64(stat.Birthtimespec.Sec), int64(stat.Birthtimespec.Nsec))
	}

	return model.Entry{
		Path:       abs,
		Name:       filepath.Base(abs),
		ParentPath: filepath.Dir(abs),
		RootPath:   filepath.Clean(root),
		Type:       entryType,
		Size:       effectiveSize(info),
		CreatedAt:  created,
		ModifiedAt: info.ModTime(),
	}, nil
}

func effectiveSize(info os.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		// st_blocks reports allocated 512-byte blocks and better reflects actual disk usage
		// for sparse disk images than logical file length.
		if stat.Blocks > 0 {
			return int64(stat.Blocks) * 512
		}
	}
	return info.Size()
}
