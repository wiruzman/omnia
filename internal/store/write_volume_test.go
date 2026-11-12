package store

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/procio"
)

type writeVolume struct {
	wal     int64
	logical uint64
	hasIO   bool
}

// With automatic checkpoints disabled, WAL length measures cumulative frames,
// not a recycled extent. Include the final database checkpoint in logical I/O.
func measureWriteVolume(t *testing.T, st *SQLiteStore, path string, action func() error) writeVolume {
	t.Helper()
	if err := st.db.Exec("PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		t.Fatal(err)
	}
	before, sampleErr := procio.CurrentSample(os.Getpid())
	if err := action(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	volume := writeVolume{wal: info.Size()}
	if err := st.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		t.Fatal(err)
	}
	after, afterErr := procio.CurrentSample(os.Getpid())
	if sampleErr == nil && afterErr == nil {
		volume.logical = after.LogicalBytesWritten - before.LogicalBytesWritten
		volume.hasIO = true
	}
	return volume
}

func shuffledWriteEntries(n int) []model.Entry {
	entries := make([]model.Entry, n)
	for i := range entries {
		path := filepath.Clean(fmt.Sprintf("/synthetic/%s/group-%03d/file-%06d.dat", strings.Repeat("directory/", 10), i%100, i))
		entries[i] = model.Entry{Path: path, Name: filepath.Base(path), ParentPath: filepath.Dir(path), RootPath: "/synthetic", Type: model.TypeFile, Size: int64(i%100) * 4096, CreatedAt: time.Unix(1700000000+int64(i), 0), ModifiedAt: time.Unix(1700000000+int64(i%50), 0)}
	}
	rand.New(rand.NewSource(7)).Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
	return entries
}
