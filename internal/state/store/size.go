package statestore

import (
	"context"
	"database/sql"
	"errors"
	"os"
)

// SizeReport reports allocation and retained fact counts. It contains no prompt
// text and makes no retention decision; free pages remain reusable by SQLite.
type SizeReport struct {
	MainBytes      int64 `json:"mainBytes"`
	WALBytes       int64 `json:"walBytes"`
	SHMBytes       int64 `json:"shmBytes"`
	TotalBytes     int64 `json:"totalBytes"`
	PageSize       int64 `json:"pageSize"`
	Pages          int64 `json:"pages"`
	FreePages      int64 `json:"freePages"`
	HostedThreads  int64 `json:"hostedThreads"`
	QueuedTurns    int64 `json:"queuedTurns"`
	SendAttempts   int64 `json:"sendAttempts"`
	CompletedTurns int64 `json:"completedTurns"`
}

func ReadSize(ctx context.Context, db *sql.DB, path string) (SizeReport, error) {
	var report SizeReport
	for _, field := range []struct {
		suffix string
		value  *int64
	}{{"", &report.MainBytes}, {"-wal", &report.WALBytes}, {"-shm", &report.SHMBytes}} {
		info, err := os.Stat(path + field.suffix)
		if field.suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return report, err
		}
		*field.value = info.Size()
	}
	report.TotalBytes = report.MainBytes + report.WALBytes + report.SHMBytes
	for _, field := range []struct {
		sql   string
		value *int64
	}{
		{"PRAGMA page_size", &report.PageSize}, {"PRAGMA page_count", &report.Pages}, {"PRAGMA freelist_count", &report.FreePages},
		{"SELECT count(*) FROM hosted_threads", &report.HostedThreads}, {"SELECT count(*) FROM queued_turns", &report.QueuedTurns},
		{"SELECT count(*) FROM send_attempts", &report.SendAttempts}, {"SELECT count(*) FROM completed_turns", &report.CompletedTurns},
	} {
		if err := db.QueryRowContext(ctx, field.sql).Scan(field.value); err != nil {
			return report, err
		}
	}
	return report, nil
}
