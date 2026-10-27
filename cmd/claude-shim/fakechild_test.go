// fakechild_test.go — the tests' stand-in for a `claude -p` child: the test
// binary re-executed with a sentinel first argument. It speaks the only two
// things the shim cares about — a `system.init` line carrying a session id,
// and one line of stdout per line of stdin — plus a few commands the tests
// drive it with (paced bursts, a controlled exit).
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	fakeChildFlag     = "-shim-fake-child"
	fakeChildLazy     = "lazy" // withhold the init line until asked for it
	fakeChildExitCode = 7
	fakeChildModel    = "fake-model"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == fakeChildFlag {
		fakeChildMain(os.Args[2:])
		return
	}
	os.Exit(m.Run())
}

// fakeChildArgs builds the argv the shim should spawn (bin is os.Args[0], the
// test binary itself — the analog of nano handing over claudeArgs()).
func fakeChildArgs(nativeID string, extra ...string) []string {
	return append([]string{fakeChildFlag, nativeID}, extra...)
}

func fakeChildMain(args []string) {
	nativeID := args[0]
	lazyInit := len(args) > 1 && args[1] == fakeChildLazy
	rawInput := len(args) > 1 && args[1] == "raw"

	emitInit := func() {
		fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":%q,\"model\":%q}\n", nativeID, fakeChildModel)
	}
	if !lazyInit {
		emitInit()
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "init":
			emitInit()
		case line == "pwd":
			cwd, _ := os.Getwd()
			fmt.Printf("{\"type\":\"pwd\",\"cwd\":%q}\n", cwd)
		case line == "entrypoint":
			fmt.Printf("{\"type\":\"entrypoint\",\"value\":%q}\n", os.Getenv(entrypointEnv))
		case line == "quit":
			fmt.Println(`{"type":"bye"}`)
			os.Exit(fakeChildExitCode)
		case strings.HasPrefix(line, "burst "):
			emitBurst(line)
		case rawInput:
			// Accept the stream-json prompt without producing output yet: the
			// parent test detaches while this provider turn remains in flight.
		default:
			fmt.Printf("{\"type\":\"echo\",\"line\":%q}\n", line)
		}
	}
}

// emitBurst handles "burst N [delayMs]": N lines, optionally paced so a test
// can attach while the child is mid-stream.
func emitBurst(command string) {
	fields := strings.Fields(command)
	count, _ := strconv.Atoi(fields[1])
	delayMs := 0
	if len(fields) > 2 {
		delayMs, _ = strconv.Atoi(fields[2])
	}
	for i := 1; i <= count; i++ {
		fmt.Printf("{\"type\":\"burst\",\"n\":%d}\n", i)
		if delayMs > 0 {
			time.Sleep(time.Duration(delayMs) * time.Millisecond)
		}
	}
}
