package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPollerBacksOffPerProviderAndRecovers(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	calls := 0
	fail := true
	poller := NewPoller(store, time.Minute, Source{Provider: "claude", Fetch: func(context.Context) *ProviderSnapshot {
		calls++
		if fail {
			return (&ProviderSnapshot{Provider: "claude"}).Fail("rate limited", nil)
		}
		return &ProviderSnapshot{Provider: "claude", ObservedAt: now, Buckets: []Bucket{{Key: "weekly", UsedPercent: 1}}}
	}})
	poller.now = func() time.Time { return now }
	poller.Poll(context.Background())
	poller.Poll(context.Background())
	if calls != 1 {
		t.Fatalf("backoff made %d calls, want 1", calls)
	}
	failures, until := poller.Backoff("claude")
	if failures != 1 || !until.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("backoff = %d/%s", failures, until)
	}
	now = until
	fail = false
	poller.Poll(context.Background())
	if calls != 2 {
		t.Fatalf("recovery calls = %d", calls)
	}
	if failures, _ := poller.Backoff("claude"); failures != 0 {
		t.Fatalf("backoff did not reset: %d", failures)
	}
}

func TestNextAlignedTickFollowsHalfHourClock(t *testing.T) {
	zone := time.FixedZone("IST", 5*3600+30*60)
	now := time.Date(2026, 9, 12, 16, 27, 13, 0, zone)
	want := time.Date(2026, 9, 12, 16, 30, 0, 0, zone)
	if got := NextAlignedTick(now, 5*time.Minute); !got.Equal(want) {
		t.Fatalf("next tick = %s, want %s", got, want)
	}
}
