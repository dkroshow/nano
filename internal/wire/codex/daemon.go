package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"
)

// ClientHandlers binds connection-scoped frames to the caller above the wire layer.
type ClientHandlers struct {
	ServerRequest func(string, string, json.RawMessage) (any, *RPCError)
	Notification  func(string, json.RawMessage)
	WireLog       WireLogger
}

// FallbackState is the connection-only state used by the supervised-daemon guard.
type FallbackState struct {
	ProcessRunning        bool
	UnavailableSince      time.Time
	LastSupervisorCheck   time.Time
	SupervisorLoaded      bool
	LastSupervisorWaitLog time.Time
}

// FallbackConfig carries the existing timing and launchd label for fallback startup.
type FallbackConfig struct {
	SpawnDelay        time.Duration
	SupervisorRate    time.Duration
	SupervisorLogRate time.Duration
	SupervisorLabel   string
	WSURL             string
}

// MaybeStartFallback applies the supervised-job guard before starting a local daemon.
func MaybeStartFallback(now time.Time, state *FallbackState, config FallbackConfig, supervisorLoaded func() bool, start func() error) error {
	if state.ProcessRunning {
		return nil
	}
	if state.UnavailableSince.IsZero() {
		state.UnavailableSince = now
	}
	if state.LastSupervisorCheck.IsZero() || now.Sub(state.LastSupervisorCheck) >= config.SupervisorRate {
		state.SupervisorLoaded = supervisorLoaded()
		state.LastSupervisorCheck = now
	}
	if state.SupervisorLoaded {
		if state.LastSupervisorWaitLog.IsZero() || now.Sub(state.LastSupervisorWaitLog) >= config.SupervisorLogRate {
			log.Printf("codex app-server unavailable on %s; supervised job %s is loaded, waiting for it instead of self-spawning", config.WSURL, config.SupervisorLabel)
			state.LastSupervisorWaitLog = now
		}
		return nil
	}
	if now.Sub(state.UnavailableSince) < config.SpawnDelay {
		return nil
	}
	return start()
}

// StartClientContext dials and initializes one Codex app-server connection.
func StartClientContext(ctx context.Context, wsURL string, clientInfo any, handlers ClientHandlers) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client := NewClientWS(wsURL)
	client.OnServerRequest = handlers.ServerRequest
	client.OnNotification = handlers.Notification
	if err := client.StartContext(ctx); err != nil {
		return nil, err
	}
	var initResp json.RawMessage
	if err := client.Call(ctx, "initialize", clientInfo, &initResp); err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := client.Notify("initialized", nil); err != nil {
		client.Close()
		return nil, err
	}
	client.WireLog = handlers.WireLog
	return client, nil
}

// WaitForClientContext retries a caller-supplied dial until it succeeds or ctx ends.
func WaitForClientContext(ctx context.Context, start func(context.Context) (*Client, error), maybeStartFallback func(time.Time) error) (*Client, error) {
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("%w (last attempt: %v)", err, lastErr)
			}
			return nil, err
		}
		client, err := start(ctx)
		if err == nil {
			return client, nil
		}
		lastErr = err
		if err := maybeStartFallback(time.Now()); err != nil {
			return nil, err
		}
		timer := time.NewTimer(150 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// StartProcessLocked starts the fallback daemon and reports its eventual exit.
func StartProcessLocked(wsURL, supervisedLabel string, newCommand func(string, ...string) *exec.Cmd, started func(*exec.Cmd), exited func(*exec.Cmd, error)) error {
	cmd := newCommand("codex", "app-server", "--listen", wsURL)
	cmd.Stdout = log.Writer()
	cmd.Stderr = log.Writer()
	if err := cmd.Start(); err != nil {
		return err
	}
	started(cmd)
	log.Printf("WARNING: self-spawned codex app-server pid %d on %s — supervised job %s is not holding the port; hosted-thread survival is incidental until it is. Check: launchctl print gui/$(id -u)/%s",
		cmd.Process.Pid, wsURL, supervisedLabel, supervisedLabel)
	go func() { exited(cmd, cmd.Wait()) }()
	return nil
}

// WaitForExit waits for one connection and reports its loss once.
func WaitForExit(client *Client, exited func()) {
	<-client.Done()
	exited()
}

// RecoverConnection runs the existing bounded-backoff daemon recovery loop.
func RecoverConnection(pending func() bool, ensure func(context.Context) error, recovered func() bool) {
	delay := 150 * time.Millisecond
	for {
		timer := time.NewTimer(delay)
		<-timer.C
		if !pending() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := ensure(ctx)
		cancel()
		if err == nil && recovered() {
			return
		}
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
}

// SupervisorLoaded reports whether launchd currently has the supervised job loaded.
func SupervisorLoaded(label string) bool {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	return exec.Command("launchctl", "print", target).Run() == nil
}
