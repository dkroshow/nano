// Command nano-launcher runs one program as its child and stays its parent.
//
// macOS attributes a launchd job's privacy requests to the job's top process
// and keys grants such as Full Disk Access to that binary's path and code
// signature. When a job runs an interpreter directly (Homebrew node for the
// Codex app server), each upgrade changes that path and hash, so grants drop.
// This small binary is signed with a fixed identity
// and sits at a fixed path, so one grant lasts. It must spawn and wait rather
// than exec: exec would make the child the top process again.
//
// It forwards termination signals, exits with the child's status, and has no
// restart logic; launchd's KeepAlive owns restarts.
//
//	nano-launcher PROGRAM [ARG...]
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: nano-launcher PROGRAM [ARG...]")
		os.Exit(2)
	}
	os.Exit(run(os.Args[1], os.Args[2:]))
}

func run(program string, args []string) int {
	cmd := exec.Command(program, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "nano-launcher: %v\n", err)
		return 1
	}
	go func() {
		for sig := range signals {
			_ = cmd.Process.Signal(sig)
		}
	}()

	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "nano-launcher: %v\n", err)
			return 1
		}
		return 0
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exitErr.ExitCode()
}
