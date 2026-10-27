package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

var cancellationFunctionID atomic.Uint64

// A deadline at the first row must neither poison the serialized writer nor
// leave a read statement holding back another connection's WAL checkpoint.
func TestDeadlineAtFirstRowReleasesStatement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	function := fmt.Sprintf("deadline_at_first_row_%d", cancellationFunctionID.Add(1))
	var queryContext context.Context
	var connection sqlite.ExecQuerierContext
	interruptObserved := false
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
		u, err := url.Parse(dsn)
		if err == nil && u.Path == path && connection == nil {
			connection = conn
		}
		return nil
	})
	err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		<-queryContext.Done()
		until := time.Now().Add(time.Second)
		for {
			// A nested statement on the executing connection observes the real
			// interrupt, without reaching into the driver's private fields.
			_, err := connection.ExecContext(context.Background(), "SELECT 1", nil)
			var sqliteError *sqlite.Error
			if errors.As(err, &sqliteError) && sqliteError.Code() == 9 {
				interruptObserved = true
				return int64(1), nil
			}
			if err != nil {
				return nil, err
			}
			if time.Now().After(until) {
				return nil, errors.New("query interrupt was not delivered")
			}
			runtime.Gosched()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	database, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("PRAGMA wal_autocheckpoint=0; CREATE TABLE t(x); INSERT INTO t VALUES(1); CREATE TABLE parent(id INTEGER PRIMARY KEY); CREATE TABLE child(parent_id INTEGER REFERENCES parent(id))"); err != nil {
		t.Fatal(err)
	}
	var cancel context.CancelFunc
	queryContext, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	rows, err := database.QueryContext(queryContext, "SELECT x, "+function+"() FROM t")
	if rows != nil {
		rows.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query error = %v, want deadline exceeded", err)
	}
	if !interruptObserved {
		t.Fatal("query did not reach the first-row cancellation boundary")
	}
	if _, err := database.Exec("INSERT INTO t VALUES(2)"); err != nil {
		t.Errorf("write after deadline: %v", err)
	}
	writer, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for range 20 {
		if _, err := writer.Exec("INSERT INTO t VALUES(3)"); err != nil {
			t.Fatal(err)
		}
	}
	var busy, total, copied int
	if err := writer.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &total, &copied); err != nil {
		t.Fatal(err)
	}
	if total <= 0 || copied != total {
		t.Errorf("checkpoint copied %d/%d frames (busy=%d), want all frames", copied, total, busy)
	}
	// Cancellation can make database/sql replace the physical connection.
	// Store settings must still apply on the next one.
	var busyTimeout int
	if err := database.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout after deadline = %d, want 5000", busyTimeout)
	}
	_, err = database.Exec("INSERT INTO child VALUES(42)")
	var sqliteError *sqlite.Error
	if !errors.As(err, &sqliteError) || sqliteError.Code() != 787 {
		t.Errorf("orphan insert after deadline: %v, want foreign-key constraint failure", err)
	}
}
