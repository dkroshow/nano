package app

import (
	"context"
	"time"

	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/usage"
	"github.com/dkroshow/nano/internal/usageclients"
)

func newUsageLimitsPoller(store *usage.Store, codex *sessions.CodexDaemon, grok config.SessionDefaults, interval time.Duration) *usage.Poller {
	var readCodex func(context.Context, string, any, any) error
	if codex != nil {
		readCodex = codex.Call
	}
	return usage.NewPoller(store, interval,
		usage.Source{Provider: "codex", Fetch: func(ctx context.Context) *usage.ProviderSnapshot {
			return usageclients.FetchCodexUsage(ctx, readCodex)
		}},
		usage.Source{Provider: "claude", Fetch: usageclients.FetchClaudeUsage},
		usage.Source{Provider: "grok", Fetch: func(ctx context.Context) *usage.ProviderSnapshot {
			return usageclients.FetchGrokUsage(ctx, grok)
		}},
	)
}
