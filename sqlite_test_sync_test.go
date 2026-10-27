package main

import (
	"context"
	"database/sql/driver"

	"modernc.org/sqlite"
)

// Root-package tests open several hundred throwaway SQLite stores under
// t.TempDir(). With the default synchronous level every commit pays a real
// fsync, which was most of the suite's long tail. No test
// can observe fsync durability: that needs an operating-system crash or power
// loss, and process-crash tests only reopen files the kernel already holds.
// This hook is compiled into the test binary only; production connections
// never see it.
func init() {
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), "PRAGMA synchronous = OFF", []driver.NamedValue{})
		return err
	})
}
