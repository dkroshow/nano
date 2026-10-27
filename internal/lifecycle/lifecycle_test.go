package lifecycle

import (
	"errors"
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func specificationState(value string) State {
	connection, work, _ := strings.Cut(value, "/")
	return State{Connection(connection), Work(work)}
}

func TestRestartRejectsOldAttachmentEvidence(t *testing.T) {
	current := Snapshot{State: State{Ready, Working}, Epoch: 10, Sequence: 99}
	steps := []struct {
		observation Observation
		want        Snapshot
		drain       bool
	}{
		{Observation{EventBoot, 11, 0}, Snapshot{State{Starting, Idle}, 11, 0}, false},
		{Observation{EventStoredState, 11, 100}, Snapshot{State{Starting, Idle}, 11, 0}, false},
		{Observation{EventReadyWorking, 10, 101}, Snapshot{State{Starting, Idle}, 11, 0}, false},
		{Observation{EventReadyIdle, 12, 1}, Snapshot{State{Starting, Idle}, 11, 0}, false},
		{Observation{EventReadyBeforeReplaySettled, 11, 1}, Snapshot{State{Starting, Idle}, 11, 0}, false},
		{Observation{EventReadyWorking, 11, 2}, Snapshot{State{Ready, Working}, 11, 2}, false},
		{Observation{EventFinished, 10, 102}, Snapshot{State{Ready, Working}, 11, 2}, false},
		{Observation{EventFinished, 11, 3}, Snapshot{State{Ready, Idle}, 11, 3}, true},
		{Observation{EventReadyIdle, 11, 4}, Snapshot{State{Ready, Idle}, 11, 4}, false},
	}
	for _, step := range steps {
		got, err := Transition(current, step.observation)
		if err != nil || got.Snapshot != step.want || got.Drain != step.drain {
			t.Fatalf("%+v: got %+v, err %v; want %+v drain %t", step.observation, got, err, step.want, step.drain)
		}
		current = got.Snapshot
	}
}

func TestOrderedEvidenceDoesNotRewindCurrentWork(t *testing.T) {
	current := Snapshot{State: State{Ready, Working}, Epoch: 7, Sequence: 20}
	for _, observation := range []Observation{
		{EventReadyIdle, 7, 0},
		{EventReadyIdle, 7, 19},
		{EventFinished, 7, 20},
		{EventStaleEvidence, 7, 21},
		{EventStoredState, 7, 22},
		{EventReadyBeforeReplaySettled, 7, 23},
	} {
		got, err := Transition(current, observation)
		if err != nil || got != (Outcome{Snapshot: current}) {
			t.Fatalf("unqualified %+v changed current work: %+v, %v", observation, got, err)
		}
	}
	got, err := Transition(current, Observation{EventLinkLost, 7, 24})
	if err != nil || got.Snapshot != (Snapshot{State{Reconnecting, Idle}, 7, 24}) || got.Drain {
		t.Fatalf("connection loss: %+v, %v", got, err)
	}
	late, err := Transition(got.Snapshot, Observation{EventReadyIdle, 7, 23})
	if err != nil || late != got {
		t.Fatalf("old ready snapshot revived a lost connection: %+v, %v", late, err)
	}
}

func TestExplicitResumeStartsANewAttachment(t *testing.T) {
	for _, connection := range []Connection{Parked, Unreachable, Detached, Exited} {
		current := Snapshot{State{connection, Idle}, 5, 42}
		got, err := Transition(current, Observation{EventResume, 6, 0})
		if err != nil || got != (Outcome{Snapshot: Snapshot{State{Starting, Idle}, 6, 0}}) {
			t.Fatalf("resume %s: %+v, %v", connection, got, err)
		}
		late, err := Transition(got.Snapshot, Observation{EventClose, 5, 43})
		if err != nil || late != got {
			t.Fatalf("old attachment close ended the resumed attachment: %+v, %v", late, err)
		}
	}
	for _, connection := range []Connection{Starting, Ready, Reconnecting} {
		current := Snapshot{State{connection, Idle}, 5, 42}
		got, err := Transition(current, Observation{EventResume, 6, 0})
		if err != nil || got != (Outcome{Snapshot: current}) {
			t.Fatalf("duplicate resume replaced an existing attachment: %+v, %v", got, err)
		}
	}
}

func TestInvalidInputPreservesStateAndEffects(t *testing.T) {
	if _, err := New(0); !errors.Is(err, ErrInvalidEpoch) {
		t.Fatalf("zero epoch constructor: %v", err)
	}
	created, err := New(8)
	if err != nil || created != (Snapshot{State{Starting, Idle}, 8, 0}) {
		t.Fatalf("new attachment: %+v, %v", created, err)
	}
	valid := Snapshot{State{Ready, Working}, 9, 17}
	cases := []struct {
		current     Snapshot
		observation Observation
		want        error
	}{
		{Snapshot{State{Connection("unknown"), Idle}, 9, 17}, Observation{EventFinished, 9, 18}, ErrInvalidState},
		{Snapshot{State{Ready, Work("unknown")}, 9, 17}, Observation{EventFinished, 9, 18}, ErrInvalidState},
		{Snapshot{State{Ready, Idle}, 0, 17}, Observation{EventReadyWorking, 0, 18}, ErrInvalidEpoch},
		{valid, Observation{Event("unknown"), 9, 18}, ErrInvalidEvent},
		{valid, Observation{EventBoot, 0, 0}, ErrInvalidEpoch},
		{valid, Observation{EventBoot, 9, 0}, ErrInvalidEpoch},
		{Snapshot{State{Detached, Idle}, 9, 17}, Observation{EventResume, 8, 0}, ErrInvalidEpoch},
		{Snapshot{State{Exited, Idle}, ^uint64(0), 17}, Observation{EventResume, 0, 0}, ErrInvalidEpoch},
	}
	for _, test := range cases {
		got, err := Transition(test.current, test.observation)
		if !errors.Is(err, test.want) || got != (Outcome{Snapshot: test.current}) {
			t.Fatalf("%+v: got %+v, err %v; want unchanged and %v", test.observation, got, err, test.want)
		}
	}
}

func TestDisconnectedLabelsUseConnection(t *testing.T) {
	for _, connection := range []Connection{Starting, Reconnecting, Parked, Unreachable, Detached, Exited} {
		// Even a malformed caller-owned value must not present stale work as a
		// disconnected thread's label. Admission separately rejects that value.
		for _, work := range []Work{Idle, Accepted, Working, WaitingOnYou} {
			state := State{connection, work}
			if got := state.Label(); got != string(connection) {
				t.Fatalf("%s labeled %q instead of its connection", state, got)
			}
		}
	}
	for _, work := range []Work{Idle, Accepted, Working, WaitingOnYou} {
		if got := (State{Ready, work}).Label(); got != string(work) {
			t.Fatalf("ready/%s labeled %q", work, got)
		}
	}
	for _, state := range []State{{Connection("unknown"), Idle}, {Ready, Work("unknown")}} {
		permission, err := Permissions(state)
		if !errors.Is(err, ErrInvalidState) || permission != (Permission{RejectInvalidState, Invalid}) || state.Label() != "invalid" {
			t.Fatalf("unknown state does not fail closed: %+v, %v, label %q", permission, err, state.Label())
		}
	}
}

func TestOnlyStandardLibraryDependencies(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			dependency, err := build.Default.Import(path, "", build.FindOnly)
			if err != nil || !dependency.Goroot {
				t.Errorf("lifecycle dependency must be standard library: %s imports %s", file, path)
			}
		}
	}
}
