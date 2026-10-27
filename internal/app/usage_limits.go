package app

import (
	"context"
	"database/sql"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/usageclients"
	"net/http"
	"path/filepath"
	"time"

	"github.com/dkroshow/nano/internal/usage"
)

const defaultUsageLimitPollInterval = usage.DefaultPollInterval

type usageBucket = usage.Bucket
type usageResetCredit = usage.ResetCredit
type usageProviderSnapshot = usage.ProviderSnapshot

type usageLimitsStore struct {
	*usage.Store
	db *sql.DB
}

func newUsageLimitsStore(path string) (*usageLimitsStore, error) {
	store, err := usage.Open(path)
	if err != nil {
		return nil, err
	}
	return &usageLimitsStore{Store: store, db: store.Database()}, nil
}

func openUsageLimits(enabled bool, catalogPath string) (*usageLimitsStore, string, error) {
	if !enabled || catalogPath == "" {
		return nil, "", nil
	}
	return openUsageLimitsAt(enabled, filepath.Join(filepath.Dir(catalogPath), "usage-limits.sqlite"))
}

func openUsageLimitsAt(enabled bool, path string) (*usageLimitsStore, string, error) {
	if !enabled || path == "" {
		return nil, "", nil
	}
	store, err := newUsageLimitsStore(path)
	return store, path, err
}

func (store *usageLimitsStore) observe(snapshot *usageProviderSnapshot) { store.Observe(snapshot) }
func (store *usageLimitsStore) snapshot() []usageProviderSnapshot       { return store.Snapshot() }

type usageLimitsPoller struct {
	*usage.Poller
	interval time.Duration
}

func newUsageLimitsPoller(store *usageLimitsStore, codex *codexDaemon, grok sessionDefaults, interval time.Duration) *usageLimitsPoller {
	if interval <= 0 {
		interval = defaultUsageLimitPollInterval
	}
	var readCodex func(context.Context, string, any, any) error
	if codex != nil {
		readCodex = codex.Call
	}
	poller := usage.NewPoller(store.Store, interval,
		usage.Source{Provider: "codex", Fetch: func(ctx context.Context) *usageProviderSnapshot {
			return usageclients.FetchCodexUsage(ctx, readCodex)
		}},
		usage.Source{Provider: "claude", Fetch: usageclients.FetchClaudeUsage},
		usage.Source{Provider: "grok", Fetch: func(ctx context.Context) *usageProviderSnapshot {
			return fetchGrokUsage(ctx, grok)
		}},
	)
	return &usageLimitsPoller{Poller: poller, interval: interval}
}

func (poller *usageLimitsPoller) run(ctx context.Context)     { poller.Run(ctx) }
func (poller *usageLimitsPoller) pollAll(ctx context.Context) { poller.Poll(ctx) }

func nextAlignedTick(now time.Time, interval time.Duration) time.Time {
	return usage.NextAlignedTick(now, interval)
}

func (s *server) handleUsageLimits(w http.ResponseWriter, r *http.Request) {
	if s.usageLimits == nil {
		httpapi.WriteJSON(w, map[string]any{"providers": []usageProviderSnapshot{}})
		return
	}
	httpapi.WriteJSON(w, map[string]any{"providers": s.usageLimits.Snapshot()})
}
