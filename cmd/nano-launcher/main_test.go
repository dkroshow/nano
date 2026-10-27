package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary act as the launcher itself, so the test
// signals a real separate launcher process, as launchd does.
func TestMain(m *testing.M) {
	if os.Getenv("NANO_LAUNCHER_TEST_AS_LAUNCHER") == "1" {
		os.Exit(run(os.Args[1], os.Args[2:]))
	}
	os.Exit(m.Run())
}

// Catches a launcher that swallows launchd's SIGTERM (the child keeps running
// and launchd must SIGKILL the job) or that reports its own status instead of
// the child's. Boundary: the launcher process as launchd sees it.
func TestSIGTERMReachesChildAndChildStatusIsReturned(t *testing.T) {
	launcher := exec.Command(os.Args[0], "/bin/sh", "-c",
		`trap 'exit 7' TERM; echo ready; while :; do sleep 0.05; done`)
	launcher.Env = append(os.Environ(), "NANO_LAUNCHER_TEST_AS_LAUNCHER=1")
	stdout, err := launcher.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Process.Kill() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child did not start: %q, %v", line, err)
	}

	if err := launcher.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- launcher.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
			t.Fatalf("launcher exit = %v, want the child's status 7", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("launcher did not exit after SIGTERM")
	}
}
