package app

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

// Codex injection: the block is additive on developerInstructions — caller
// text first and verbatim — and baseInstructions is never touched, because it
// replaces the built-in system prompt rather than extending it.
func TestCodexIdentityInjection(t *testing.T) {
	base := spawnRequest{providerRequest: providerRequest{Provider: "codex", Cwd: "/w", Approval: "never", Sandbox: "workspace-write",
		HostedThreadID: "nht_test123", apiOrigin: "http://127.0.0.1:7999"},
	}

	t.Run("block alone when the caller sent none", func(t *testing.T) {
		p := codexThreadParams(base.providerRequest)
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
		p := codexThreadParams(req.providerRequest)
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
		if _, ok := codexThreadParams(req.providerRequest)["developerInstructions"]; ok {
			t.Fatal("preview-posture request minted an identity block")
		}
	})
	t.Run("no origin (resume-resolved request) means no block", func(t *testing.T) {
		req := base
		req.apiOrigin = ""
		if _, ok := codexThreadParams(req.providerRequest)["developerInstructions"]; ok {
			t.Fatal("origin-less request minted an identity block")
		}
	})
}

// Claude injection: the identity flag rides argv as a singleton pair, absent
// whenever the request cannot honestly name a hosted identity.
func TestClaudeArgsIdentityFlag(t *testing.T) {
	req := spawnRequest{providerRequest: providerRequest{Provider: "claude", HostedThreadID: "nht_claude1", apiOrigin: "http://127.0.0.1:7999"}}
	args := claudeArgs(req.providerRequest)
	block := singletonValue(t, args, claudeIdentityFlag)
	if !strings.Contains(block, "hostedThreadId: nht_claude1") ||
		!strings.Contains(block, "provider: claude") {
		t.Fatalf("identity block = %q, want hosted id and provider", block)
	}

	for name, strip := range map[string]func(r spawnRequest) spawnRequest{
		"no hosted id": func(r spawnRequest) spawnRequest { r.HostedThreadID = ""; return r },
		"no origin":    func(r spawnRequest) spawnRequest { r.apiOrigin = ""; return r },
	} {
		t.Run(name, func(t *testing.T) {
			for _, a := range claudeArgs(strip(req).providerRequest) {
				if a == claudeIdentityFlag {
					t.Fatalf("argv minted an identity flag without %s", name)
				}
			}
		})
	}
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

// End to end through spawnSession: a New Window (UI-path) claude start reaches
// the shim carrying --append-system-prompt with the session's own minted
// hosted id — proving both the identity flag and the hostedThreadID
// write-back that feeds it (slice-1 checklist items 1 and 3).
func TestClaudeStartSpawnCarriesIdentity(t *testing.T) {
	// A fresh start, so the shim behaves as a real one does: it sends the init
	// only after Nano has adopted its cs_ id (startingShim).
	hold, speak := make(chan struct{}), make(chan struct{})
	close(hold)
	shim := startingShim(t, hold, speak)
	s, _ := claudeResumeServer(t, shim.wsURL())
	migrateTestControl(t, s.controlMetadata)
	s.defaults.apiOrigin = "http://127.0.0.1:7999"

	sess, err := s.spawnSession(spawnRequest{providerRequest: providerRequest{Provider: "claude", Cwd: t.TempDir()}})
	if err != nil {
		t.Fatalf("start spawn: %v", err)
	}
	provisionallyAdopted(t, sess, speak)
	waitForResumedThreadID(t, s, sess.id, "native-1")

	if sess.hostedThreadID == "" {
		t.Fatal("start spawn minted no hosted thread id")
	}
	_, args, _ := startArgs(t, shim)
	block := singletonValue(t, args, claudeIdentityFlag)
	if !strings.Contains(block, "hostedThreadId: "+sess.hostedThreadID) {
		t.Fatalf("injected block = %q, want the session's own hosted id %s", block, sess.hostedThreadID)
	}
	if !strings.Contains(block, "http://127.0.0.1:7999/api/v1/hosted-threads/"+sess.hostedThreadID) {
		t.Fatalf("injected block = %q, want the self-lookup URL", block)
	}
}

// A cs_ shim stand-in must never pass as a real provider id: the view marks
// it provisional, and a genuine UUID clears the marker. Live instance that
// motivated this: the grok-acp-projector worker record of 2026-08-05.
func TestAgentSessionViewMarksProvisionalNativeID(t *testing.T) {
	_, store := newTestHostedControl(t)
	hosted, _, err := store.beginSession("caller", "hash", "claude", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, agentExecutions: store}

	if err := store.bindSession(hosted.ID, "nano-1", "cs_eec9373f24115d539dd1c06786b96b36", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	if view := s.agentSessionView(hosted.ID); !view.NativeThreadIDProvisional {
		t.Fatalf("view = %+v, want the cs_ stand-in marked provisional", view)
	}

	resolveTestProvisional(t, store.control, hosted.ID, "claude", "cs_eec9373f24115d539dd1c06786b96b36", "2dd36191-9386-542b-a1ef-bed95e638620")
	if err := store.bindSession(hosted.ID, "nano-1", "2dd36191-9386-542b-a1ef-bed95e638620", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	if view := s.agentSessionView(hosted.ID); view.NativeThreadIDProvisional {
		t.Fatalf("view = %+v, want a real UUID unmarked", view)
	}
}

// singletonValue asserts flag appears exactly once in args and returns its
// value.
func singletonValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	value, count := "", 0
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			count++
			if i+1 < len(args) {
				value = args[i+1]
			}
		}
	}
	if count != 1 {
		t.Fatalf("argv %#v carries %d %s flags, want exactly 1", args, count, flag)
	}
	return value
}
