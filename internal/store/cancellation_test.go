package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"unsafe"

	"github.com/wiruzman/omnia/internal/store/sqlitetest"
)

func cancellationDB(t *testing.T) *sqliteDB {
	t.Helper()
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "cancellation.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := db.Exec("CREATE TABLE fixture (value INTEGER);"); err != nil {
		t.Fatal(err)
	}
	return db
}

func assertTransactionReusable(t *testing.T, db *sqliteDB, wantRows int64) {
	t.Helper()
	if !sqlitetest.AutoCommit(unsafe.Pointer(db.db)) {
		t.Fatal("transaction leaked")
	}
	if err := db.WithTx(context.Background(), func() error {
		return db.Exec("INSERT INTO fixture VALUES (1);")
	}); err != nil {
		t.Fatalf("reuse transaction: %v", err)
	}
	stmt, err := db.Prepare("SELECT count(*) FROM fixture;")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Finalize()
	row, err := stmt.Step()
	if err != nil || !row || stmt.ColumnInt64(0) != wantRows {
		t.Fatalf("rows=%d want=%d row=%v err=%v", stmt.ColumnInt64(0), wantRows, row, err)
	}
}

func TestSQLiteInterruptedBeginRollsBackBeforeReuse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		autocommit bool
	}{{"before_transaction_opens", true}, {"after_transaction_opens", false}} {
		t.Run(tc.name, func(t *testing.T) {
			db := cancellationDB(t)
			fired, clear := sqlitetest.InterruptOnAutoCommit(unsafe.Pointer(db.db), tc.autocommit, 1)
			t.Cleanup(clear)
			called := false
			err := db.WithTx(context.Background(), func() error { called = true; return nil })
			if err == nil || !strings.Contains(err.Error(), "interrupted") || called || fired() != 1 {
				t.Fatalf("err=%v body called=%v interrupts=%d", err, called, fired())
			}
			assertTransactionReusable(t, db, 1)
		})
	}
}

func TestSQLiteInterruptedRollbackRetriesBeforeReuse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		autocommit bool
	}{{"before_transaction_closes", false}, {"after_transaction_closes", true}} {
		t.Run(tc.name, func(t *testing.T) {
			db := cancellationDB(t)
			bodyErr := errors.New("transaction body failed")
			var fired func() int
			err := db.WithTx(context.Background(), func() error {
				if err := db.Exec("INSERT INTO fixture VALUES (2);"); err != nil {
					return err
				}
				var clear func()
				fired, clear = sqlitetest.InterruptOnAutoCommit(unsafe.Pointer(db.db), tc.autocommit, 1)
				t.Cleanup(clear)
				return bodyErr
			})
			if err != bodyErr || fired == nil || fired() != 1 {
				t.Fatalf("unexpected rollback result: %v (armed=%v)", err, fired != nil)
			}
			assertTransactionReusable(t, db, 1) // The failed transaction's insert is gone.
		})
	}
}

func TestSQLiteRollbackFailureIsReportedAndRetryBounded(t *testing.T) {
	db := cancellationDB(t)
	bodyErr := errors.New("transaction body failed")
	var fired func() int
	var clear func()
	err := db.WithTx(context.Background(), func() error {
		fired, clear = sqlitetest.InterruptOnAutoCommit(unsafe.Pointer(db.db), false, 3)
		return bodyErr
	})
	if fired == nil {
		t.Fatalf("transaction body did not run: %v", err)
	}
	defer func() {
		clear()
		_ = db.Exec("ROLLBACK;")
	}()
	if !errors.Is(err, bodyErr) || !strings.Contains(err.Error(), "rollback sqlite transaction") || fired() != 2 {
		t.Fatalf("err=%v interrupts=%d", err, fired())
	}
	if err := db.WithTx(context.Background(), func() error { t.Error("ran with leaked transaction"); return nil }); err == nil {
		t.Fatal("expected active-transaction error after unrecoverable rollback")
	}
}

func TestSQLiteCancellationStopJoinsLateCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		release := make(chan struct{})
		calls := 0
		stop := sqliteInterruptOnCancel(ctx, func() { <-release; calls++ })
		cancel()
		synctest.Wait() // The callback is parked immediately before interrupting.
		stopped := false
		go func() { stop(); stopped = true }()
		synctest.Wait()
		premature := stopped
		if premature {
			t.Error("stop returned while the old callback could still interrupt a later operation")
		}
		close(release)
		synctest.Wait()
		if !stopped || calls != 1 {
			t.Fatalf("stopped=%v callbacks=%d", stopped, calls)
		}
		if !premature {
			stop() // Safe for the transaction's deferred cleanup to call again.
		}
	})
}

func TestSQLiteCancellationAfterStopCannotReachReusedConnection(t *testing.T) {
	db := cancellationDB(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		stop := db.interruptOnCancel(ctx)
		stop()
		cancel()
		synctest.Wait()
		stop()
	})
	assertTransactionReusable(t, db, 1)
}

func TestSQLiteCanceledTransactionRollsBackAndPreservesContextError(t *testing.T) {
	db := cancellationDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := db.WithTx(ctx, func() error {
		if err := db.Exec("INSERT INTO fixture VALUES (2);"); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	assertTransactionReusable(t, db, 1)
}

func TestSQLiteAlreadyCanceledTransactionDoesNotRun(t *testing.T) {
	db := cancellationDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := db.WithTx(ctx, func() error { t.Error("canceled transaction body ran"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	assertTransactionReusable(t, db, 1)
}

func TestSQLiteNestedTransactionDoesNotRollbackOuter(t *testing.T) {
	db := cancellationDB(t)
	err := db.WithTx(context.Background(), func() error {
		if err := db.Exec("INSERT INTO fixture VALUES (2);"); err != nil {
			return err
		}
		if err := db.WithTx(context.Background(), func() error {
			t.Error("nested transaction body ran")
			return nil
		}); err == nil {
			t.Error("nested transaction should fail")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertTransactionReusable(t, db, 2)
}
