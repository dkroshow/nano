package main

import (
	"context"
	"database/sql"
	state "github.com/dkroshow/nano/internal/app/statestore"
	"log"
	"time"
)

func watchThreadsSize(ctx context.Context, db *sql.DB, path string, interval time.Duration, report func(state.SizeReport, error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		size, err := state.ReadSize(ctx, db, path)
		report(size, err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func startThreadsSizeWatch(db *sql.DB, path string) {
	go watchThreadsSize(context.Background(), db, path, time.Hour, func(size state.SizeReport, err error) {
		if err != nil {
			log.Printf("threads size watch: %v", err)
			return
		}
		log.Printf("threads size: main=%d wal=%d shm=%d total=%d freePages=%d/%d hosted=%d queue=%d attempts=%d completed=%d", size.MainBytes, size.WALBytes, size.SHMBytes, size.TotalBytes, size.FreePages, size.Pages, size.HostedThreads, size.QueuedTurns, size.SendAttempts, size.CompletedTurns)
	})
}
