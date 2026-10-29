package sessions

import (
	"reflect"
	"strings"
	"testing"
)

// The shared argv-singleton helper (coordination decision 12): claude --resume
// succession, identity-flag dedup, and the posture overlay all lean on these
// exact mechanics.
func TestArgvSingletonHelpers(t *testing.T) {
	t.Run("set strips repeats and appends once", func(t *testing.T) {
		got := argvSetSingletonPair([]string{"--resume", "a", "-p", "--resume", "b"}, "--resume", "t9")
		want := []string{"-p", "--resume", "t9"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("set = %#v, want %#v", got, want)
		}
	})
	t.Run("set tolerates a dangling flag", func(t *testing.T) {
		got := argvSetSingletonPair([]string{"-p", "--resume"}, "--resume", "t9")
		want := []string{"-p", "--resume", "t9"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("set = %#v, want %#v", got, want)
		}
	})
	t.Run("collapse keeps the last value once", func(t *testing.T) {
		got := argvCollapseSingletonPair([]string{"-x", "1", "-p", "-x", "2"}, "-x")
		want := []string{"-p", "-x", "2"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("collapse = %#v, want %#v", got, want)
		}
	})
	t.Run("collapse without the flag is unchanged", func(t *testing.T) {
		got := argvCollapseSingletonPair([]string{"-p", "--verbose"}, "-x")
		want := []string{"-p", "--verbose"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("collapse = %#v, want %#v", got, want)
		}
	})
	t.Run("collapse drops a dangling valueless flag", func(t *testing.T) {
		got := argvCollapseSingletonPair([]string{"-p", "-x"}, "-x")
		want := []string{"-p"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("collapse = %#v, want %#v", got, want)
		}
	})
}

// Resume replay: the stored block survives exactly once, no matter how the
// stored argv accumulated, and never gains a fresh value.
func TestClaudeResumeArgsKeepsSingleIdentityPair(t *testing.T) {
	stored := []string{
		"-p", "--verbose", claudeIdentityFlag, "block-v1",
		claudeResumeFlag, "native-0",
	}
	got := claudeResumeArgs(stored, "t9")
	want := []string{"-p", "--verbose", claudeIdentityFlag, "block-v1", claudeResumeFlag, "t9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume argv = %#v, want %#v", got, want)
	}

	duplicated := []string{
		claudeIdentityFlag, "block-old", "-p", claudeIdentityFlag, "block-new",
	}
	got = claudeResumeArgs(duplicated, "t9")
	want = []string{"-p", claudeIdentityFlag, "block-new", claudeResumeFlag, "t9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deduped argv = %#v, want %#v", got, want)
	}
}

// Codex injection: the block is additive on developerInstructions — caller
// text first and verbatim — and baseInstructions is never touched, because it
// replaces the built-in system prompt rather than extending it.
func TestCodexIdentityInjection(t *testing.T) {
	base := ProviderRequest{Provider: "codex", Cwd: "/w", Approval: "never", Sandbox: "workspace-write",
		HostedThreadID: "nht_test123", APIOrigin: "http://127.0.0.1:7999"}

	t.Run("block alone when the caller sent none", func(t *testing.T) {
		p := codexThreadParams(base)
		dev, _ := p["developerInstructions"].(string)
		if !strings.Contains(dev, "hostedThreadId: nht_test123") ||
			!strings.Contains(dev, "http://127.0.0.1:7999/api/v1/hosted-threads/nht_test123") ||
			!strings.Contains(dev, "/inbox?actorThreadRef=nht_test123") ||
			!strings.Contains(dev, "acted, declined, or deferred") {
			t.Fatalf("developerInstructions = %q, want the identity block", dev)
		}
	})
	t.Run("caller text survives verbatim ahead of the block", func(t *testing.T) {
		req := base
		req.DeveloperInstructions = "caller rules"
		req.BaseInstructions = "caller base"
		p := codexThreadParams(req)
		dev, _ := p["developerInstructions"].(string)
		if !strings.HasPrefix(dev, "caller rules\n\n") || !strings.Contains(dev, "nht_test123") {
			t.Fatalf("developerInstructions = %q, want caller text first, block appended", dev)
		}
		if p["baseInstructions"] != "caller base" {
			t.Fatalf("baseInstructions = %v, want untouched caller value", p["baseInstructions"])
		}
	})
	t.Run("no hosted id (preview posture) means no block", func(t *testing.T) {
		req := base
		req.HostedThreadID = ""
		if _, ok := codexThreadParams(req)["developerInstructions"]; ok {
			t.Fatal("preview-posture request minted an identity block")
		}
	})
	t.Run("no origin (resume-resolved request) means no block", func(t *testing.T) {
		req := base
		req.APIOrigin = ""
		if _, ok := codexThreadParams(req)["developerInstructions"]; ok {
			t.Fatal("origin-less request minted an identity block")
		}
	})
}

// Claude injection: the identity flag rides argv as a singleton pair, absent
// whenever the request cannot honestly name a hosted identity.
func TestClaudeArgsIdentityFlag(t *testing.T) {
	req := ProviderRequest{Provider: "claude", HostedThreadID: "nht_claude1", APIOrigin: "http://127.0.0.1:7999"}
	args := claudeArgs(req)
	block := singletonValue(t, args, claudeIdentityFlag)
	if !strings.Contains(block, "hostedThreadId: nht_claude1") ||
		!strings.Contains(block, "provider: claude") {
		t.Fatalf("identity block = %q, want hosted id and provider", block)
	}

	for name, strip := range map[string]func(r ProviderRequest) ProviderRequest{
		"no hosted id": func(r ProviderRequest) ProviderRequest { r.HostedThreadID = ""; return r },
		"no origin":    func(r ProviderRequest) ProviderRequest { r.APIOrigin = ""; return r },
	} {
		t.Run(name, func(t *testing.T) {
			for _, a := range claudeArgs(strip(req)) {
				if a == claudeIdentityFlag {
					t.Fatalf("argv minted an identity flag without %s", name)
				}
			}
		})
	}
}
