package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

type fallbackCommand struct {
	name string
	args []string
	at   time.Time
}

func recordFallbackCommands(commands chan<- fallbackCommand) func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		commands <- fallbackCommand{name: name, args: append([]string(nil), args...), at: time.Now()}
		return exec.Command("/bin/sleep", "1")
	}
}

func reserveLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func startWebSocketProxy(t *testing.T, address, target string) {
	t.Helper()
	targetURL, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: httputil.NewSingleHostReverseProxy(targetURL)}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
}

func TestCodexDaemonWaitsForLoadedSupervisorAndReconnects(t *testing.T) {
	commands := make(chan fallbackCommand, 1)
	address := reserveLoopbackAddress(t)
	daemon := newCodexDaemon("ws://"+address, supervisedDaemonLabel)
	daemon.supervisedJobLoaded = func() bool { return true }
	daemon.fallbackSpawnDelay = 20 * time.Millisecond
	daemon.newCommand = recordFallbackCommands(commands)
	sess := reconnectTestSession(t, daemon, "loaded-supervisor-thread")
	daemon.mu.Lock()
	daemon.reconnectPending = true
	daemon.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	err := daemon.ensureContext(ctx)
	cancel()
	if err == nil {
		t.Fatal("ensureContext unexpectedly connected while the supervised port was empty")
	}
	select {
	case command := <-commands:
		t.Fatalf("fallback spawned while supervised job was loaded: %s %v", command.name, command.args)
	default:
	}

	fake := newFakeAppServer(t)
	startWebSocketProxy(t, address, fake.srv.URL)
	if err := daemon.ensureContext(context.Background()); err != nil {
		t.Fatalf("connect after supervised port returned: %v", err)
	}
	waitReconnectCondition(t, func() bool {
		return fake.resumeCount() == 1 && sess.summary().Status == "ready"
	})
}

func TestCodexDaemonWaitsBeforeFallbackSpawnAndPreservesArgv(t *testing.T) {
	commands := make(chan fallbackCommand, 1)
	daemon := newCodexDaemon("ws://127.0.0.1:1", supervisedDaemonLabel)
	daemon.supervisedJobLoaded = func() bool { return false }
	daemon.fallbackSpawnDelay = 250 * time.Millisecond
	daemon.newCommand = recordFallbackCommands(commands)

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	done := make(chan error, 1)
	go func() { done <- daemon.ensureContext(ctx) }()

	var command fallbackCommand
	select {
	case command = <-commands:
	case <-time.After(750 * time.Millisecond):
		cancel()
		t.Fatal("fallback did not spawn after the supervised job was reported absent")
	}
	if elapsed := command.at.Sub(started); elapsed < daemon.fallbackSpawnDelay {
		cancel()
		t.Fatalf("fallback spawned after %s, before the %s supervisor restart guard", elapsed, daemon.fallbackSpawnDelay)
	}
	if command.name != "codex" {
		cancel()
		t.Fatalf("fallback program = %q, want codex", command.name)
	}
	wantArgs := []string{"app-server", "--listen", daemon.wsURL}
	if !reflect.DeepEqual(command.args, wantArgs) {
		cancel()
		t.Fatalf("fallback args = %#v, want %#v", command.args, wantArgs)
	}

	cancel()
	if err := <-done; err == nil {
		t.Fatal("ensureContext unexpectedly connected to the empty fallback port")
	}
}
