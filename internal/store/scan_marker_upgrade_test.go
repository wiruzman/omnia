package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/wiruzman/omnia/internal/store/sqlitetest"
)

// Version 1 used the same marker rows and deletion trigger, but indexed the
// changing generation. Keep this fixture explicit so upgrades and I/O controls
// exercise the released 0.1.5 index, not an alternative production path.
func installVersionOneMarkerIndex(t *testing.T, st *SQLiteStore) {
	t.Helper()
	if err := st.db.Exec(`DROP INDEX scan_markers_root_idx;
		CREATE INDEX scan_markers_root_scan_idx ON scan_markers(root_path,last_seen_scan);
		PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaWorkUsesFileStorageAndRestoresRuntimeMode(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			var st *SQLiteStore
			if version == 0 {
				st = legacyMarkerStore(t, filepath.Join(t.TempDir(), "index.sqlite"))
				// Include the older FTS rebuilds, which run after the schema
				// transaction and must also retain file-backed temporary storage.
				for _, name := range []string{"name", "path"} {
					if err := st.db.Exec(fmt.Sprintf("DROP TABLE %s_fts; CREATE VIRTUAL TABLE %s_fts USING fts5(%s_lower, tokenize='trigram');", name, name, name)); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				st = changeStore(t)
				installVersionOneMarkerIndex(t, st)
			}
			counts, clear := sqlitetest.ObserveSchemaStorage(unsafe.Pointer(st.db.db))
			defer clear()
			if err := st.setup(); err != nil {
				t.Fatal(err)
			}
			file, other := counts()
			if file == 0 || other != 0 {
				t.Fatalf("schema work used wrong storage: file=%d other=%d", file, other)
			}
			if mode := markerScalar(t, st, "PRAGMA temp_store;"); mode != 2 {
				t.Fatalf("runtime temp_store not restored to MEMORY: %d", mode)
			}
		})
	}
}

func TestVersionOneMarkerUpgradePreservesDataWithoutRewrites(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	if err := st.UpsertBatch(ctx, 42, changeEntries(3)); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePath(ctx, changeEntries(3)[1].Path); err != nil {
		t.Fatal(err)
	}
	installVersionOneMarkerIndex(t, st)
	before := markerScalar(t, st, "SELECT total_changes();")
	entriesPage := markerScalar(t, st, "SELECT rootpage FROM sqlite_master WHERE name='entries';")
	ftsPage := markerScalar(t, st, "SELECT rootpage FROM sqlite_master WHERE name='path_fts_data';")
	if err := st.setup(); err != nil {
		t.Fatal(err)
	}
	if markerScalar(t, st, "SELECT total_changes();") != before {
		t.Fatal("index-only upgrade rewrote data")
	}
	if markerScalar(t, st, "SELECT rootpage FROM sqlite_master WHERE name='entries';") != entriesPage || markerScalar(t, st, "SELECT rootpage FROM sqlite_master WHERE name='path_fts_data';") != ftsPage {
		t.Fatal("entry/FTS tables were rebuilt")
	}
	if markerScalar(t, st, "SELECT count(*) FROM scan_markers WHERE entry_id IN(1,3) AND last_seen_scan=42;") != 2 {
		t.Fatal("marker IDs or generations changed")
	}
	assertStaticMarkerSchema(t, st)
	assertMarkerIntegrity(t, st)
	// The existing deletion trigger must not be duplicated or lost on upgrade.
	if err := st.DeletePath(ctx, changeEntries(1)[0].Path); err != nil {
		t.Fatal(err)
	}
	assertMarkerIntegrity(t, st)
}

func TestVersionOneMarkerUpgradeRollsBackAndRetries(t *testing.T) {
	st := changeStore(t)
	if err := st.UpsertBatch(context.Background(), 7, changeEntries(3)); err != nil {
		t.Fatal(err)
	}
	installVersionOneMarkerIndex(t, st)
	// Force CREATE INDEX to fail after DROP INDEX. Rollback must restore the
	// released index and version, not leave a partially upgraded database.
	if err := st.db.Exec("CREATE TABLE scan_markers_root_idx(collision);"); err != nil {
		t.Fatal(err)
	}
	if err := st.setup(); err == nil {
		t.Fatal("expected index-name collision")
	}
	if markerScalar(t, st, "PRAGMA user_version;") != 1 || markerScalar(t, st, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='scan_markers_root_scan_idx';") != 1 {
		t.Fatal("failed upgrade escaped rollback")
	}
	if markerScalar(t, st, "PRAGMA temp_store;") != 2 {
		t.Fatal("failed upgrade did not restore runtime storage")
	}
	if markerScalar(t, st, "SELECT count(*) FROM scan_markers WHERE last_seen_scan=7;") != 3 {
		t.Fatal("failed upgrade changed markers")
	}
	if err := st.db.Exec("DROP TABLE scan_markers_root_idx;"); err != nil {
		t.Fatal(err)
	}
	if err := st.setup(); err != nil {
		t.Fatal(err)
	}
	assertStaticMarkerSchema(t, st)
	assertMarkerIntegrity(t, st)
}

func assertStaticMarkerSchema(t *testing.T, st *SQLiteStore) {
	t.Helper()
	if markerScalar(t, st, "PRAGMA user_version;") != scanMarkerSchemaVersion {
		t.Fatal("marker schema version not advanced")
	}
	if markerScalar(t, st, "SELECT count(*) FROM sqlite_master WHERE name='scan_markers_root_scan_idx';") != 0 {
		t.Fatal("changing generation index remains")
	}
	if markerScalar(t, st, "SELECT count(*) FROM pragma_index_info('scan_markers_root_idx');") != 1 {
		t.Fatal("static root index has unexpected columns")
	}
	if markerScalar(t, st, "SELECT count(*) FROM pragma_index_info('scan_markers_root_idx') WHERE name='root_path';") != 1 {
		t.Fatal("marker index does not contain only root_path")
	}
}
