package scanner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/wiruzman/omnia/internal/model"
)

func TestUnreadableCallbackOnlyReportsInspectionFailures(t *testing.T) {
	var paths []string
	options := WalkOptions{OnUnreadable: func(path string) error { paths = append(paths, path); return nil }}
	for _, err := range []error{fs.ErrNotExist, syscall.ENOTDIR} {
		if err := options.unreadable("missing", err); err != nil {
			t.Fatal(err)
		}
	}
	if len(paths) != 0 {
		t.Fatal("missing paths were protected")
	}
	for _, err := range []error{fs.ErrPermission, syscall.EIO} {
		if err := options.unreadable("unreadable", err); err != nil {
			t.Fatal(err)
		}
	}
	if len(paths) != 2 || !filepath.IsAbs(paths[0]) {
		t.Fatalf("inspection failures not reported: %v", paths)
	}
	want := errors.New("preservation failed")
	options.OnUnreadable = func(string) error { return want }
	if err := options.unreadable("unreadable", fs.ErrPermission); !errors.Is(err, want) {
		t.Fatalf("callback error lost: %v", err)
	}
}

func TestWalkReportsUnreadableRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	if _, err := os.Stat(root); !os.IsPermission(err) {
		t.Skip("platform can stat inaccessible root")
	}
	var unreadable []string
	err := New(nil).WalkWithOptions([]string{root}, func(model.Entry) error { t.Fatal("unreadable root emitted entries"); return nil }, nil, func(error) {}, WalkOptions{OnUnreadable: func(path string) error { unreadable = append(unreadable, path); return nil }})
	if err != nil || len(unreadable) != 1 || unreadable[0] != root {
		t.Fatalf("root protection: %v err=%v", unreadable, err)
	}
}
