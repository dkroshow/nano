package sessions

import (
	"reflect"
	"testing"
)

func TestClaudeCanonicalResumeArgsPreservesTransportAllowlist(t *testing.T) {
	stored := []string{
		"-p", "--model", "old-model", "--effort", "low", "--permission-mode", "default",
		"--allowed-tools", "Read", "--allowed-tools", "Grep", claudeResumeFlag, "native-1",
	}
	got := ClaudeCanonicalResumeArgs(stored, ProviderRequest{Model: "canonical-model", Effort: "high", PermissionMode: "bypassPermissions"})
	if containsArgPair(got, claudeModelFlag, "old-model") || containsArgPair(got, "--effort", "low") || containsArgPair(got, "--permission-mode", "default") {
		t.Fatalf("stale canonical fields survived: %#v", got)
	}
	if !containsArgPair(got, claudeModelFlag, "canonical-model") || !containsArgPair(got, "--effort", "high") || !containsArgPair(got, "--permission-mode", "bypassPermissions") {
		t.Fatalf("canonical fields were not applied: %#v", got)
	}
	if !containsArgPair(got, "--allowed-tools", "Read") || !containsArgPair(got, "--allowed-tools", "Grep") {
		t.Fatalf("transport allow-list was removed: %#v", got)
	}
}

// Anything the translator does not recognize still reaches the operator, and a
// result carrying no message at all still says the handshake never happened.
func TestClaudeSpawnFailureFallsBackToTheChildsOwnWords(t *testing.T) {
	cases := []struct {
		name string
		errs []string
		want string
	}{
		{"unrecognized", []string{"Credit balance is too low"}, "claude could not start the session: Credit balance is too low"},
		{"several", []string{"a", "b"}, "claude could not start the session: a; b"},
		{"silent", nil, "claude exited before the session handshake"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeSpawnFailure("native-1", tc.errs); got != tc.want {
				t.Fatalf("claudeSpawnFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

// claudeResumeArgs is the anti-stacking rule on its own: any number of stored
// `--resume` pairs collapse into exactly the current target, appended last.
func TestClaudeResumeArgsStripsStaleResume(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"no stored resume", []string{"-p", "--verbose"}, []string{"-p", "--verbose", "--resume", "t9"}},
		{"strips the pair", storedPosture, []string{"-p", "--verbose", "--permission-mode", "bypassPermissions", "--resume", "t9"}},
		{"repeated pairs", []string{"--resume", "a", "-p", "--resume", "b"}, []string{"-p", "--resume", "t9"}},
		{"dangling flag", []string{"-p", "--resume"}, []string{"-p", "--resume", "t9"}},
		{"empty posture", nil, []string{"--resume", "t9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeResumeArgs(tc.in, "t9"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("claudeResumeArgs(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
