package usage

import (
	"context"
	"sync"
	"time"
)

const (
	DefaultPollInterval = 5 * time.Minute
	maxFailureBackoff   = 30 * time.Minute
	tickSkew            = 150 * time.Millisecond
)

type Source struct {
	Provider string
	Fetch    func(context.Context) *ProviderSnapshot
}

type sourceState struct {
	failures int
	next     time.Time
}

type Poller struct {
	store    *Store
	interval time.Duration
	sources  []Source
	now      func() time.Time

	mu    sync.Mutex
	state map[string]sourceState
}

func NewPoller(store *Store, interval time.Duration, sources ...Source) *Poller {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	return &Poller{
		store: store, interval: interval, sources: append([]Source(nil), sources...),
		now: time.Now, state: make(map[string]sourceState),
	}
}

func (poller *Poller) Interval() time.Duration {
	if poller == nil || poller.interval <= 0 {
		return DefaultPollInterval
	}
	return poller.interval
}

func (poller *Poller) Run(ctx context.Context) {
	if poller == nil || poller.store == nil {
		return
	}
	poller.Poll(ctx)
	for {
		timer := time.NewTimer(time.Until(NextAlignedTick(time.Now(), poller.interval)) + tickSkew)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			poller.Poll(ctx)
		}
	}
}

func (poller *Poller) Poll(ctx context.Context) {
	if poller == nil || poller.store == nil {
		return
	}
	var wait sync.WaitGroup
	for _, source := range poller.sources {
		source := source
		if source.Provider == "" || source.Fetch == nil {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			poller.attempt(ctx, source)
		}()
	}
	wait.Wait()
}

func (poller *Poller) attempt(ctx context.Context, source Source) {
	now := poller.now()
	poller.mu.Lock()
	state := poller.state[source.Provider]
	if !state.next.IsZero() && now.Before(state.next) {
		poller.mu.Unlock()
		poller.store.RecordAttempt(Attempt{
			At: now.UTC(), Provider: source.Provider,
			Note: "backing off after provider failures until " + state.next.UTC().Format(time.RFC3339),
		})
		return
	}
	poller.mu.Unlock()

	started := poller.now()
	snapshot := source.Fetch(ctx)
	if snapshot == nil {
		snapshot = (&ProviderSnapshot{Provider: source.Provider, Buckets: []Bucket{}}).Fail("Usage source returned no snapshot", nil)
	}
	if snapshot.Provider == "" {
		snapshot.Provider = source.Provider
	}
	poller.store.RecordAttempt(Attempt{
		At: started.UTC(), Provider: source.Provider, OK: snapshot.Error == "",
		DurationMS: poller.now().Sub(started).Milliseconds(), Error: snapshot.Error,
		Detail: snapshot.ErrorDetail, Note: snapshot.Notice, Buckets: len(snapshot.Buckets),
	})
	poller.store.Observe(snapshot)

	poller.mu.Lock()
	defer poller.mu.Unlock()
	state = poller.state[source.Provider]
	if snapshot.Error == "" {
		delete(poller.state, source.Provider)
		return
	}
	state.failures++
	delay := poller.interval
	for count := 0; count < state.failures; count++ {
		if delay >= maxFailureBackoff/2 {
			delay = maxFailureBackoff
			break
		}
		delay *= 2
	}
	state.next = now.Add(delay)
	poller.state[source.Provider] = state
}

func NextAlignedTick(now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	_, offset := now.Zone()
	shift := time.Duration(offset) * time.Second
	next := now.Add(shift).Truncate(interval).Add(interval).Add(-shift)
	if next.Sub(now) < time.Second {
		next = next.Add(interval)
	}
	return next
}

func (poller *Poller) Backoff(provider string) (failures int, until time.Time) {
	if poller == nil {
		return 0, time.Time{}
	}
	poller.mu.Lock()
	defer poller.mu.Unlock()
	state := poller.state[provider]
	return state.failures, state.next
}
