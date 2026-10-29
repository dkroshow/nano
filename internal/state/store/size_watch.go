package statestore

import (
	"context"
	"database/sql"
	"log"
	"time"
)


// WatchSize reports database growth until cancellation.
func WatchSize(ctx context.Context, db *sql.DB, path string, interval time.Duration, report func(SizeReport, error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		size, err := ReadSize(ctx, db, path)
		report(size, err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// StartSizeWatch reports canonical file sizes once per hour.
func StartSizeWatch(db *sql.DB, path string) {
	go WatchSize(context.Background(), db, path, time.Hour, func(size SizeReport, err error) {
		if err != nil {
			log.Printf("threads size watch: %v", err)
			return
		}
		log.Printf("threads size: main=%d wal=%d shm=%d total=%d freePages=%d/%d hosted=%d queue=%d attempts=%d completed=%d", size.MainBytes, size.WALBytes, size.SHMBytes, size.TotalBytes, size.FreePages, size.Pages, size.HostedThreads, size.QueuedTurns, size.SendAttempts, size.CompletedTurns)
	})
}
