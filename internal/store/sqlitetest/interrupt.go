// Package sqlitetest supplies deterministic SQLite fault injection to store tests.
// It is not imported by production code.
package sqlitetest

/*
#cgo LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
	sqlite3 *db;
	int remaining;
	int fired;
	int autocommit;
} interruption;

static int interrupt_transaction(void *arg) {
	interruption *state = arg;
	if (state->remaining > 0 && sqlite3_get_autocommit(state->db) == state->autocommit) {
		state->remaining--;
		state->fired++;
		return 1;
	}
	return 0;
}

static interruption *arm_interrupt(sqlite3 *db, int autocommit, int times) {
	interruption *state = calloc(1, sizeof(interruption));
	state->db = db;
	state->remaining = times;
	state->autocommit = autocommit;
	sqlite3_progress_handler(db, 1, interrupt_transaction, state);
	return state;
}

static int deny_alter(void *unused, int action, const char *a, const char *b,
                      const char *database, const char *source) {
	return action == SQLITE_ALTER_TABLE ? SQLITE_DENY : SQLITE_OK;
}

static void deny_alter_table(sqlite3 *db) {
	sqlite3_set_authorizer(db, deny_alter, NULL);
}

typedef struct {
	int file_mode;
	int file_ddl;
	int other_ddl;
} schema_storage;

static int observe_schema_storage(void *arg, int action, const char *a,
                                  const char *b, const char *database, const char *source) {
	schema_storage *state = arg;
	if (action == SQLITE_PRAGMA && a && b && !strcmp(a, "temp_store")) {
		state->file_mode = !sqlite3_stricmp(b, "FILE");
	}
	switch (action) {
	case SQLITE_ALTER_TABLE:
	case SQLITE_CREATE_INDEX:
	case SQLITE_CREATE_TABLE:
	case SQLITE_CREATE_VTABLE:
	case SQLITE_CREATE_TRIGGER:
	case SQLITE_DROP_INDEX:
	case SQLITE_DROP_TABLE:
		if (state->file_mode) state->file_ddl++;
		else state->other_ddl++;
	}
	return SQLITE_OK;
}

static schema_storage *watch_schema_storage(sqlite3 *db) {
	schema_storage *state = calloc(1, sizeof(schema_storage));
	sqlite3_set_authorizer(db, observe_schema_storage, state);
	return state;
}
*/
import "C"

import "unsafe"

// InterruptOnAutoCommit interrupts the next times VM steps observed with the
// requested autocommit state, including partially executed BEGIN or ROLLBACK.
// Callers must not use these controls concurrently with the connection.
func InterruptOnAutoCommit(db unsafe.Pointer, enabled bool, times int) (fired func() int, clear func()) {
	mode := 0
	if enabled {
		mode = 1
	}
	state := C.arm_interrupt((*C.sqlite3)(db), C.int(mode), C.int(times))
	return func() int { return int(state.fired) }, func() {
		C.sqlite3_progress_handler((*C.sqlite3)(db), 0, nil, nil)
		C.free(unsafe.Pointer(state))
	}
}

func AutoCommit(db unsafe.Pointer) bool {
	return C.sqlite3_get_autocommit((*C.sqlite3)(db)) != 0
}

// DenyAlterTable injects a deterministic failure after migration has copied its
// data, without changing the schema or crashing a process mid-transaction.
func DenyAlterTable(db unsafe.Pointer) func() {
	C.deny_alter_table((*C.sqlite3)(db))
	return func() { C.sqlite3_set_authorizer((*C.sqlite3)(db), nil, nil) }
}

// ObserveSchemaStorage records the temp-store mode at schema operations without
// timing or memory-size assertions. As with fault injection, use one connection
// and remove the authorizer before reusing it for another observer.
func ObserveSchemaStorage(db unsafe.Pointer) (counts func() (file, other int), clear func()) {
	state := C.watch_schema_storage((*C.sqlite3)(db))
	return func() (int, int) { return int(state.file_ddl), int(state.other_ddl) }, func() {
		C.sqlite3_set_authorizer((*C.sqlite3)(db), nil, nil)
		C.free(unsafe.Pointer(state))
	}
}
