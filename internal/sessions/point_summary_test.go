package sessions

import (
	"reflect"
	"testing"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// A point reference must build only its matching summary, preserving the fleet
// ordering (including timestamp precision), fields and exited-session filter.
func TestAgentExecutionLiveHostedSessionSelectsOnlyMatchingSummary(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	selected := &Session{
		id: "a", provider: "codex", hostedThreadID: "target", threadID: "native", displayName: "display",
		model: "model", cwd: "/fixture", createdAt: created.Add(900 * time.Millisecond),
		invokerHostedThreadID: "parent", invokerNativeThreadID: "parent-native", invokerProvider: "claude",
		sidebarPlacement: threadvalues.SidebarPlacementStandalone, spawnMode: "resume", spawnError: "error",
		lifecycleState: testLifecycleSnapshot("ready", true), turnStartedAtMs: 1234,
		metadataDegraded: true, turnEffort: "high", permissionMode: "never", turnServiceTier: "fast",
	}
	unrelated := &Session{id: "unrelated", hostedThreadID: "other", lifecycleState: testLifecycleSnapshot("ready", false)}
	registry := NewRegistry()
	for _, sess := range map[string]*Session{
		"a":         selected,
		"z":         {id: "z", hostedThreadID: "target", createdAt: created.Add(100 * time.Millisecond), lifecycleState: testLifecycleSnapshot("ready", false)},
		"exited":    {id: "exited", hostedThreadID: "target", createdAt: created.Add(-time.Hour), lifecycleState: testLifecycleSnapshot("exited", false)},
		"unrelated": unrelated,
	} {
		registry.RegisterSession(sess)
	}

	unrelated.stateMu.Lock()
	done := make(chan struct{})
	var got Summary
	var found bool
	go func() {
		got, found = registry.LiveHostedSummary("target", func(string) bool { return true })
		close(done)
	}()
	select {
	case <-done:
		unrelated.stateMu.Unlock()
	case <-time.After(time.Second):
		unrelated.stateMu.Unlock()
		<-done
		t.Fatal("point reference waited for an unrelated session's summary")
	}
	want := Summary{
		ID: "a", Provider: "codex", HostedThreadID: "target", ThreadID: "native", Name: "display",
		Model: "model", Cwd: "/fixture", Status: "ready", CreatedAt: "2026-01-01T00:00:00Z",
		InvokerHostedThreadID: "parent", InvokerNativeThreadID: "parent-native", InvokerProvider: "claude",
		PresentationMode: presentationShared, SidebarPlacement: threadvalues.SidebarPlacementStandalone, SpawnMode: "resume",
		Error: "error", TurnActive: true, ThreadStatus: "working", TurnStartedAtMs: 1234, NeedsAttention: true, MetadataDegraded: true,
		Posture: hostedstore.Posture{Model: "model", Effort: "high", PermissionMode: "never", ServiceTier: "fast"},
	}
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("point summary=%+v found=%v want=%+v", got, found, want)
	}
	if _, found := registry.LiveHostedSummary("missing", func(string) bool { t.Fatal("missing session requested attention"); return false }); found {
		t.Fatal("unknown hosted thread resolved to a live session")
	}
}
