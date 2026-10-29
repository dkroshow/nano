package statestore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

func TestThreadsSizeWatchReportsGrowthAndStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nano_threads.sqlite")
	_, err := PrepareStore(context.Background(), StorePaths{Destination: path, BackupRoot: filepath.Join(t.TempDir(), "backups")}, testMigrations())
	if err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(context.Background(), path)
	if err == nil {
		t.Cleanup(func() { db.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := make(chan SizeReport, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSize(ctx, db, path, 10*time.Millisecond, func(size SizeReport, err error) {
			if err == nil {
				select {
				case rows <- size:
				case <-ctx.Done():
				}
			}
		})
	}()
	var before SizeReport
	select {
	case before = <-rows:
	case <-time.After(time.Second):
		t.Fatal("initial size report missing")
	}
	if err := prompts.SaveQueuedTurns(context.Background(), db, []prompts.QueuedTurn{{ClientActionID: "growth", Text: strings.Repeat("x", 64*1024)}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case size := <-rows:
			if size.QueuedTurns != 1 || size.TotalBytes <= before.TotalBytes {
				continue
			}
			cancel()
			select {
			case <-done:
				return
			case <-time.After(time.Second):
				t.Fatal("size watch did not stop")
			}
		case <-deadline:
			t.Fatal("size watch did not report retained data growth")
		}
	}
}
