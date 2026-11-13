package store

import "fmt"

const scanMarkerSchemaVersion = 1

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
	// This is the only marker deletion owner, shared by every entry-delete path.
	// It also rolls back together with entry and FTS changes on transaction failure.
	return s.db.Exec(`CREATE INDEX scan_markers_root_scan_idx ON scan_markers(root_path, last_seen_scan);
		CREATE TRIGGER entries_delete_scan_marker AFTER DELETE ON entries BEGIN
			DELETE FROM scan_markers WHERE entry_id=OLD.rowid;
		END;
		PRAGMA user_version=1;`)
}
