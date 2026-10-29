package app

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestAsyncTurnStoreFailureReachesTheThreadDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	p := newLiveProjection("codex", "hosted-thread")
	cfg := runtimeFixtureConfig(t, sessionConfig{ID: "async-diagnostics", Provider: "codex", HostedThreadID: "hosted-thread", Projection: p})
	f := newProviderRuntimeFixture(t, cfg, "native-thread", "")
	if err := cfg.QueuedTurns.Database().Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.emit(t, liveTextDeltaTurnCompleted("completed"))
	}
	f.emitAndWait(t, []byte(`{"method":"fixture/drained","params":{"threadId":"native-thread"}}`))

	diagnostics := p.ThreadDiagnosticsSnapshot()
	if len(diagnostics) != 1 || diagnostics[0].Code != "async_session_event_failed" ||
		diagnostics[0].Message != "An asynchronous session event could not be applied." || diagnostics[0].Occurrences != 3 {
		t.Fatalf("async failure diagnostics = %#v", diagnostics)
	}
	if got := strings.Count(logs.String(), "thread diagnostic session"); got != 1 {
		t.Fatalf("bounded diagnostic log count = %d, want 1; logs=%q", got, logs.String())
	}
	if strings.Contains(logs.String(), "core session async event failed") {
		t.Fatalf("async failures bypassed bounded diagnostic logging: %q", logs.String())
	}
}

func TestLiveTextDeltasEnvironmentSwitch(t *testing.T) {
	t.Setenv("NANO_LIVE_TEXT_DELTAS", "")
	if liveTextDeltasDisabled() {
		t.Fatal("unset switch disabled live text deltas")
	}
	t.Setenv("NANO_LIVE_TEXT_DELTAS", " OFF ")
	if !liveTextDeltasDisabled() {
		t.Fatal("NANO_LIVE_TEXT_DELTAS=off not honored")
	}
	s := &server{dropLiveTextDeltas: liveTextDeltasDisabled()}
	if !s.newSessionProjection("codex", "hosted").DropsLiveTextDeltas() {
		t.Fatal("server setting did not reach the session projection")
	}
}
