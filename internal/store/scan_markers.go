package store

import "fmt"

const scanMarkerSchemaVersion = 2

// migrateScanMarkers runs inside setup's schema transaction. Keep rowids and
// scan generations intact: FTS references and committed resume cursors use them.
func (s *SQLiteStore) migrateScanMarkers() error {
	stmt, err := s.db.Prepare("PRAGMA user_version;")
	if err != nil {
		return err
	}
	row, err := stmt.Step()
	version := stmt.ColumnInt64(0)
	stmt.Finalize()
	if err != nil {
		return err
	}
	if !row || version < 0 || version > scanMarkerSchemaVersion {
		return fmt.Errorf("unsupported index schema version: %d", version)
	}
	if version == scanMarkerSchemaVersion {
		return nil
	}
	if version == 0 {
		stmt, err = s.db.Prepare("SELECT 1 FROM pragma_table_info('entries') WHERE name='last_seen_scan';")
		if err != nil {
			return err
		}
		legacy, err := stmt.Step()
		stmt.Finalize()
		if err != nil {
			return err
		}
		if err := s.db.Exec(`CREATE TABLE scan_markers (
		entry_id INTEGER PRIMARY KEY,
		root_path TEXT NOT NULL,
		last_seen_scan INTEGER NOT NULL
		);`); err != nil {
			return err
		}
		if legacy {
			if err := s.db.Exec(`INSERT INTO scan_markers SELECT rowid, root_path, last_seen_scan FROM entries;
			DROP INDEX IF EXISTS entries_root_scan_idx;
			ALTER TABLE entries DROP COLUMN last_seen_scan;`); err != nil {
				return err
			}
		}
		// The only marker deletion owner, shared by every entry-delete path.
		if err := s.db.Exec(`CREATE TRIGGER entries_delete_scan_marker AFTER DELETE ON entries BEGIN
			DELETE FROM scan_markers WHERE entry_id=OLD.rowid;
		END;`); err != nil {
			return err
		}
	}
	// Version 1 already has durable markers. Replace only its changing index;
	// never rewrite entries, FTS, or scan generations on that upgrade path.
	return s.db.Exec(`DROP INDEX IF EXISTS scan_markers_root_scan_idx;
		CREATE INDEX scan_markers_root_idx ON scan_markers(root_path);
		PRAGMA user_version=2;`)
}
