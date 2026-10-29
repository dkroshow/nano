package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/core/prompts"
)

func TestThreadsSizeWatchReportsGrowthAndStops(t *testing.T) {
	db, path, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := make(chan state.SizeReport, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchThreadsSize(ctx, db, path, 10*time.Millisecond, func(size state.SizeReport, err error) {
			if err == nil {
				select {
				case rows <- size:
				case <-ctx.Done():
				}
			}
		})
	}()
	var before state.SizeReport
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
