package scanner

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"omnia-search-tui/internal/model"
)

func TestShouldExclude(t *testing.T) {
	s := New([]string{".git", "node_modules", "Library/Caches"})
	if !s.ShouldExclude("/Users/u/repo/.git/config") {
		t.Fatal("expected .git path to be excluded")
	}
	if s.ShouldExclude("/Users/u/repo/src/main.go") {
		t.Fatal("did not expect src/main.go to be excluded")
	}
}

func TestWalkCollectsEntriesAndSkipsExcluded(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "node_modules", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "keep", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "node_modules", "x", "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New([]string{"node_modules"})
	var entries []model.Entry
	err := s.Walk([]string{tmp}, func(e model.Entry) error {
		entries = append(entries, e)
		return nil
	}, nil, func(error) {})
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if filepath.Base(e.Path) == "b.txt" {
			t.Fatal("excluded file was scanned")
		}
	}
}

func TestWalkMissingRootWarns(t *testing.T) {
	s := New(nil)
	warned := false
	err := s.Walk([]string{"/does/not/exist"}, func(model.Entry) error { return nil }, nil, func(error) { warned = true })
	if err != nil {
		t.Fatal(err)
	}
	if !warned {
		t.Fatal("expected warning for missing root")
	}
}

func TestWalkWithOptionsResumesFromRootAndPath(t *testing.T) {
	tmp := t.TempDir()
	r1 := filepath.Join(tmp, "r1")
	r2 := filepath.Join(tmp, "r2")
	if err := os.MkdirAll(r1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r2, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(r1, "a.txt"),
		filepath.Join(r1, "b.txt"),
		filepath.Join(r2, "a.txt"),
		filepath.Join(r2, "b.txt"),
		filepath.Join(r2, "c.txt"),
	} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := New(nil)
	resumeAfter := filepath.Join(r2, "b.txt")
	files := make([]string, 0)
	err := s.WalkWithOptions([]string{r1, r2}, func(e model.Entry) error {
		if e.Type == model.TypeFile {
			files = append(files, filepath.Base(e.Path))
		}
		return nil
	}, nil, func(error) {}, WalkOptions{ResumeRoot: r2, ResumeAfterPath: resumeAfter})
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(files)
	if len(files) != 1 || files[0] != "c.txt" {
		t.Fatalf("expected only c.txt after resume, got %+v", files)
	}
}
