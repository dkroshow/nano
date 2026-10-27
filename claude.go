package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/lifecycle"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	transcriptmodel "github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const claudeCloseGrace = 3 * time.Second
const claudeBinEnv = "CODEX_NANO_CLAUDE_BIN"

// Nano signs every Claude child it spawns. claude CLI 2.1.220 honors
// CLAUDE_CODE_ENTRYPOINT and writes the value into its transcript rows as
// `entrypoint`, which the catalog stores as the thread's origin and classifies
// (internal/history/interaction.go): unsigned children read `sdk-cli` →
// programmatic → hidden behind the default Explorer filter the moment their
// session is closed. A thread nano starts or resumes is operator work and must
// never be hidden. The shim daemon spawns nano's other children and stamps the
// same pair — cmd/claude-shim/session.go, which is deliberately dependency-free
// and so repeats the literals rather than importing them.
const (
	claudeEntrypointEnv  = "CLAUDE_CODE_ENTRYPOINT"
	claudeNanoEntrypoint = "nano"
)

// claudeResumeFlag replays a past session into a fresh child — the second rung
// of the resume ladder, used when the shim no longer holds the live one.
const claudeResumeFlag = "--resume"

// claudeTransport carries one hosted session's stdio. Two implementations: the
// shim daemon (the child outlives nano) and a direct child process (it does
// not). Everything above this seam — wire recording, init capture, status
// transitions — is transport-agnostic, so turnActiveTransition, the catalog
// nudges and the whole frontend dialect see identical traffic either way.
type claudeTransport interface {
	// sendLine delivers one stream-json line to the child's stdin.
	sendLine(line []byte) error
	// isReplaying reports whether the lines currently arriving are a spool
	// replay rather than live traffic. Only the shim transport ever replays;
	// a direct child has no history to re-deliver. The control channel needs
	// this because a replayed inbound request belongs to a previous nano's
	// session and must not be parked as live (claude_control.go); the turn
	// anchor needs it because a replayed turn-opening line must not be dated
	// to the moment it was re-delivered (D2/D8).
	isReplaying() bool
	// lastSeq is the highest sequence number the daemon has assigned to a line
	// this transport has seen. A direct child has no sequence space and always
	// reports 0, which is also the correct "no evidence" anchor seq.
	lastSeq() int64
	// close ends the session, giving the child the chance to exit cleanly and
	// flush its native transcript.
	close() error
	// durableAttachment builds the attachment record to remember once the
	// native session id lands, or reports false when the session dies with
	// nano — remembering a doomed child would be a lie (attachments.go).
	durableAttachment(nativeThreadID, model string) (attachmentRecord, bool)
}

type claudeContextLineSender interface {
	sendLineContext(context.Context, []byte) error
}

// claudeSpawnPosture is the {binary, argv} a child was started with. It is
// transport recovery input for a later `--resume` fallback; canonical desired
// metadata, not the stored flags, decides any hosted model override.
type claudeSpawnPosture struct {
	Bin  string
	Args []string
}

type claudeSession struct {
	sess *session
	// initCausation is captured while wiring the child, before it can emit
	// system.init.  A replay is explicitly denied this snapshot below.
	initCausation providerCausationSnapshot
	// tr is written once at start, before the session is published, and is
	// never read by a transport's own read loop (which passes itself down).
	tr claudeTransport
	// supersedes is the native session id this one replaces, set on both resume
	// rungs. An attach lands on the same id and changes nothing; a `--resume`
	// child mints a new one, which retires the old attachment record
	// (captureInit). Empty for a fresh start.
	supersedes string

	// initSeen records that the child's system.init handshake actually landed.
	// It is what makes a later failing result "one turn's error" rather than a
	// failed spawn, and it is deliberately not derived from sess.threadID: a
	// `--resume` child is seeded with its predecessor's id before any wire
	// traffic (resumeClaudeSession), so id presence no longer implies a
	// handshake. Caveat, unchanged from before this flag existed: the attach
	// rung learns nothing from a spool that holds no init line, so a session
	// whose replay starts after the handshake still reads a later failing
	// result as a spawn failure.
	initSeen atomic.Bool

	// control correlates the session's control-channel traffic — interrupt,
	// set_model, initialize — with its responses (claude_control.go). Created
	// with the session so a line arriving before start finishes has somewhere
	// to land.
	control *claudeControl

	// storedAnchor is the turn anchor read off the attachment record BEFORE
	// this session attached — the replay's own captureInit rewrites the record
	// wholesale, so reading it later would find it already erased.
	storedAnchor attachmentRecord

	// replayTurnSeq is the shim sequence number of the newest turn-opening user
	// line seen during the replay, cleared by the result line that closes that
	// turn. Non-zero when the replay ends therefore means exactly "a turn is
	// still in flight, and this is the line that opened it" — the evidence the
	// stored anchor is revalidated against. Written only from the shim client's
	// single read loop; the replayDone close orders it against the reader.
	replayTurnSeq atomic.Int64

	// anchorSeq is the shim sequence number nano had already seen when it sent
	// the live turn's user line. Sampled BEFORE the send, which is what makes
	// the replay check an inequality that cannot race: every seq the shim
	// assigns afterwards, the user line's own included, is strictly greater.
	anchorSeq atomic.Int64

	// persistedAnchor is the anchor last written to the attachment record, so
	// the common case (every line that is not a turn boundary) costs a compare
	// instead of a file rewrite. anchorMu serialises the compare-and-write: the
	// send path and the read loop both sync, and interleaving them could leave
	// the record describing a turn the session no longer has.
	anchorMu        sync.Mutex
	persistedAnchor atomic.Int64

	// turnIngressMu closes the fresh-start acknowledgment race. The shim can
	// deliver init/output/result notifications before its session/input response
	// reaches Nano. While one operation waits for that response, deliverLine
	// parks those frames. A successful send first records the one outbound user
	// boundary; the operation then persists its exact turn binding and releases
	// the parked frames. This keeps a fast result on claude:1 instead of letting
	// it close claude:1 before a late local boundary opens claude:2.
	turnIngressMu      sync.Mutex
	turnIngressHeld    bool
	turnIngressPending []claudeIngressLine
	lastLiveWireAt     time.Time
	// unanswered holds the submission ids of prompts Nano wrote that no
	// Claude result has named yet (noteAnswered).
	unansweredMu sync.Mutex
	unanswered   map[string]bool
	// unnamedLoggedFor is the native transcript for which this attachment
	// has logged a result naming no prompt while one was unanswered: a sign
	// of a CLI format change, logged once rather than per result. Read and
	// written only where frames apply.
	unnamedLoggedFor string

	exited   chan struct{}
	exitOnce sync.Once
}

type claudeIngressLine struct {
	tr        claudeTransport
	dir       protocol.Direction
	line      []byte
	seq       int64
	at        time.Time
	replaying bool
	turn      claudeFrameTurn
	// then runs right after the line is applied: a result's terminal event,
	// which must follow its own frame.
	then func()
}

// claudeFrameTurn is the shim's exact turn identity for one spooled frame: the
// provider turn id of the Nano prompt it belongs to, or empty for work no Nano
// prompt started (a background-task notification turn). The projection takes
// it per frame, in apply order, so an unprompted turn is never stamped with the
// previous prompt's id. A direct child has no such identity (known false).
type claudeFrameTurn struct {
	id    string
	known bool
}

// claudeQueuedWorkGate is implemented by a transport whose queued frames can
// be abandoned: a failed attach turns them into no-ops on the session loop.
type claudeQueuedWorkGate interface {
	queuedWorkAllowed() error
	admitted()
}

// claudePermissionPromptFlag routes permission asks and the interactive-tool
// family to the control channel on stdio. It is undocumented (absent from
// `--help`) and it does two things, only one of which is obvious:
//
//  1. a tool needing approval arrives as a `can_use_tool` control request the
//     operator can answer, instead of being auto-denied with "Claude requested
//     permissions to use Bash, but you haven't granted it yet";
//  2. **it is what puts `AskUserQuestion`, `EnterPlanMode` and `ExitPlanMode`
//     into the advertised tool list at all.** Without it those tools do not
//     exist in `-p` stream-json mode — probed 2026-07-31 across both permission
//     modes: 30 tools without the flag, 33 with it, and the three extra ones are
//     exactly the interactive family. This corrects slice 3a finding §7, which
//     read their absence as "AskUserQuestion does not exist in -p mode".
//
// Only new children get it. Stored postures in attachments.json carry their own
// argv, so the fleet heals as sessions are restarted rather than needing a
// migration.
var claudePermissionPromptFlag = []string{"--permission-prompt-tool", "stdio"}

// claudeNoToolsSentinel is an internal request value used by Nano's private
// status classifier. Claude's explicit `--tools ""` disables the advertised
// tool set; an empty AllowedTools field would instead omit all restriction.
const claudeNoToolsSentinel = "__nano_status_no_tools__"

// claudeArgs builds the exact argv (after the binary name) for a resolved
// spawn request. Used by both startClaudeSession and /api/sessions/preview,
// so the New Window dialog's command pane cannot drift from what runs.
func claudeArgs(req spawnRequest) []string {
	args := []string{
		"-p", "--verbose",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		// No --replay-user-messages: our own send lines are already in the
		// wire buffer at the right position; the echo arrives late and
		// misorders the conversation.
	}
	args = append(args, claudePermissionPromptFlag...)
	if req.Model != "" {
		args = append(args, claudeModelFlag, req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.PermissionMode != "" {
		args = append(args, "--permission-mode", req.PermissionMode)
	}
	if req.AllowedTools == claudeNoToolsSentinel {
		args = append(args, "--tools", "")
	} else {
		for _, tool := range strings.Split(req.AllowedTools, ",") {
			if tool = strings.TrimSpace(tool); tool != "" {
				args = append(args, "--allowed-tools", tool)
			}
		}
	}
	if identityInjectable(req) {
		args = append(args, claudeIdentityFlag,
			nanoIdentityContext(req.HostedThreadID, req.Provider, req.apiOrigin))
	}
	return args
}

// claudeIdentityFlag carries the injected identity block (agent_identity.go).
// Like --resume it is a singleton pair: resume replay collapses any repeats so
// the argv cannot grow a second block on succession.
const claudeIdentityFlag = "--append-system-prompt"

// claudeResumeArgs turns a stored posture into resume argv. Any `--resume` pair
// left by an earlier fallback is dropped before the current target is appended:
// resuming a session that was itself resumed would otherwise stack flags,
// growing the argv on every hop and misreporting what the child ran with. The
// identity flag follows the same singleton rule via the shared argv helper
// (argv_singleton.go), keeping the stored block while refusing duplicates.
func claudeResumeArgs(args []string, threadID string) []string {
	out := argvCollapseSingletonPair(args, claudeIdentityFlag)
	return argvSetSingletonPair(out, claudeResumeFlag, threadID)
}

// claudeModelFlag is the model pair on the child argv — what claudeArgs writes
// at spawn and the resume overlay rewrites at respawn.
const claudeModelFlag = "--model"

// claudeCanonicalResumeArgs removes canonicalized posture flags from an old
// attachment command and reapplies the hosted thread's canonical state and
// immutable Nano identity.
// Attachment argv remains a transport fragment for protocol, resume lineage,
// and --allowed-tools: Nano does not yet model a lossless tool allow-list, so
// preserving it avoids silently narrowing the resumed child's capabilities.
func claudeCanonicalResumeArgs(args []string, req spawnRequest) []string {
	without := argvDropPairs(args, map[string]bool{
		claudeModelFlag: true, "--effort": true, "--permission-mode": true,
	})
	if req.Model != "" {
		without = argvSetSingletonPair(without, claudeModelFlag, req.Model)
	}
	if req.Effort != "" {
		without = argvSetSingletonPair(without, "--effort", req.Effort)
	}
	if req.PermissionMode != "" {
		without = argvSetSingletonPair(without, "--permission-mode", req.PermissionMode)
	}
	if identityInjectable(req) {
		without = argvSetSingletonPair(without, claudeIdentityFlag,
			nanoIdentityContext(req.HostedThreadID, req.Provider, req.apiOrigin))
	}
	return without
}

func argvDropPairs(args []string, flags map[string]bool) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if flags[args[i]] {
			i++ // every flag in this set has exactly one value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// applyClaudeResumeOverlay applies canonical desired intent over the stored
// transport argv. An explicit desired model survives confirmed or rejected
// attempts on a new attachment; a cleared intent adds no override.
func applyClaudeResumeOverlay(args []string, source *controlMetadataStore, hostedThreadID string) []string {
	if source == nil {
		return args
	}
	view, err := source.view(hostedThreadID)
	if err != nil {
		return args
	}
	value, _ := canonicalResumeString(view.Fields[postureFieldModel])
	if value == "" {
		return args
	}
	return argvSetSingletonPair(args, claudeModelFlag, value)
}

// claudeCommand is the argv a spawn request would run, for the New Window
// dialog's command pane. A resume previews its `--resume` fallback rung: the
// attach rung it tries first runs no command at all.
func claudeCommand(req spawnRequest) []string {
	if req.ClaudeBin == "" {
		req.ClaudeBin = resolveClaudeBin("")
	}
	if req.Mode == "resume" {
		return append([]string{req.ClaudeBin}, claudeResumeArgs(req.ClaudeArgs, req.ThreadID)...)
	}
	return append([]string{req.ClaudeBin}, claudeArgs(req)...)
}

func resolveClaudeBin(configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	if env := strings.TrimSpace(os.Getenv(claudeBinEnv)); env != "" {
		return env
	}
	candidates := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "claude"))
	}
	candidates = append(candidates, "/opt/homebrew/bin/claude", "/usr/local/bin/claude")
	for _, candidate := range candidates {
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() && fi.Mode()&0111 != 0 {
			return candidate
		}
	}
	if path, err := exec.LookPath("claude"); err == nil {
		return path
	}
	return "claude"
}

// startClaudeSession begins streaming a hosted session's wire into the
// session's event buffer. The session id arrives asynchronously on the
// system.init line.
func startClaudeSession(sess *session, req spawnRequest) (*claudeSession, error) {
	if req.ClaudeBin == "" {
		req.ClaudeBin = resolveClaudeBin("")
	}
	posture := claudeSpawnPosture{Bin: req.ClaudeBin, Args: claudeArgs(req)}
	return openClaudeSession(sess, req.ClaudeShimWS, posture, "", sess.snapshotProviderCausation(claudeOutboundFacts(posture.Args)))
}

// resumeClaudeSession re-attaches a detached claude session, walking the
// fallback ladder (docs/decisions/claude-shim-daemon.md):
//
//  1. the shim still hosts the child → attach from the replay floor that the
//     durable prompt facts and the spool prove (claude_replay_floor.go), the
//     one rung that keeps an in-flight turn alive. Unprovable evidence stops
//     here with errClaudeRecoveryUnproven;
//  2. the shim lost it (its own restart takes its children with it) or cannot
//     be reached → a fresh child with `--resume <id>`, which restores the
//     conversation from the native transcript but not a turn in flight;
//  3. someone else holds the session → refuse. Falling back here would spawn a
//     second child racing the first over one transcript.
func resumeClaudeSession(sess *session, req spawnRequest) (*claudeSession, error) {
	posture := claudeSpawnPosture{Bin: req.ClaudeBin, Args: req.ClaudeArgs}
	// Identity at spawn, the codex idiom (codex.go's pre-claim): take the thread
	// id before any wire traffic, whichever rung ends up hosting the session.
	// The attach rung normally re-derives it from the replayed init — but a
	// never-prompted thread has no init in its spool, and its provisional
	// identity (the shim handle, adoptDurableIdentity) must survive the
	// re-attach or the record it is keyed by becomes unforgettable. The
	// `--resume` rung announces its own brand-new id only later — under claude
	// CLI 2.1.220 not until it has been sent a turn — and until then the session
	// would match no thread at all: no dual-attachment guard, no runtime
	// overlay, no tab identity. captureInit replaces this with the real id when
	// it lands.
	sess.stateMu.Lock()
	sess.threadID = req.ThreadID
	sess.stateMu.Unlock()
	sess.setProjectionThreadID(req.ThreadID)
	// A normal resume knows its real id before any wire traffic, so write a
	// dialog-supplied name now. A never-prompted child carries only a provisional
	// shim handle: keep its restored New Window title pending until the first
	// turn's init supplies the native id, or we would persist the name under an
	// identity the catalog will immediately retire.
	if !req.ClaudeAnchor.provisional() {
		sess.applyPendingName(req.ThreadID)
	}
	if shimURL := strings.TrimSpace(req.ClaudeShimWS); shimURL != "" {
		// The replay floor comes from durable prompt facts and the shim's
		// spool, never from the stored turn anchor (claude_replay_floor.go).
		plan, err := sess.planClaudeReplay(context.Background(), req.ClaudeShimSpools, req.ThreadID, req.ClaudeAnchor)
		if err != nil && plan.Resolved != "" {
			// The spool proves which session the provisional handle's own
			// prompts landed in. That pair is recorded before any frame is
			// admitted or any rung chosen, and the resume continues as that
			// native; if it cannot be recorded, nothing proceeds.
			anchor, adoptErr := sess.adoptResolvedClaudeNative(req.ThreadID, plan.Resolved, req.ClaudeAnchor)
			if adoptErr != nil {
				return nil, adoptErr
			}
			req.ThreadID, req.ClaudeAnchor = plan.Resolved, anchor
			plan, err = sess.planClaudeReplay(context.Background(), req.ClaudeShimSpools, req.ThreadID, req.ClaudeAnchor)
		}
		if err != nil {
			plan = claudeReplayPlan{FromSeq: claudeShimTailSeq, Err: err, SpoolBytes: plan.SpoolBytes}
		}
		c, err := attachClaudeSession(sess, shimURL, req.ThreadID, plan, posture, req.ClaudeAnchor)
		if err == nil {
			return c, nil
		}
		if errors.Is(err, errClaudeRecoveryUnproven) {
			// The child may be alive and working; a replacement would be a
			// second writer. Fail visibly and leave the record for a retry.
			return nil, err
		}
		if errors.Is(err, errShimSessionAttached) {
			return nil, fmt.Errorf("claude thread %s is attached elsewhere: %w", req.ThreadID, err)
		}
		if errors.Is(err, errSpawnSessionClosed) {
			return nil, err // the session is gone; a fallback child would answer to nobody
		}
		if errors.Is(err, errClaudeReplayUnsettled) {
			// Attach established that the original child exists. A local replay
			// failure must leave it available for retry, not create a second writer.
			return nil, err
		}
		if req.ClaudeAnchor.provisional() {
			// The thread was never prompted, so no native transcript exists and
			// the `--resume` rung below does not apply. The shim either answered
			// that the child is gone (its own restart took it — the empty
			// conversation is unrecoverable, and the caller should retire the
			// record) or could not be reached (the child may still be alive —
			// leave the record for a later attempt).
			if errors.Is(err, errShimSessionNotFound) {
				// Only positive evidence that nothing was ever handed to the
				// provider lets the caller retire the record: a prompt that
				// was or may have been delivered keeps it for recovery.
				if delivered, proofErr := sess.provisionalMayHaveDelivered(context.Background(), req.ThreadID); delivered || proofErr != nil {
					return nil, fmt.Errorf("%w: claude thread %s may have been prompted under %s and its child is gone (%v): %v", errClaudeRecoveryUnproven, sess.hostedThreadID, req.ThreadID, proofErr, err)
				}
				return nil, fmt.Errorf("%w: %v", errClaudeProvisionalGone, err)
			}
			return nil, fmt.Errorf("claude thread %s was never prompted and can only be re-attached, not respawned: %w", req.ThreadID, err)
		}
		if req.externalResumePosture {
			return nil, fmt.Errorf("cannot establish exclusive Claude attachment for external conversation %s: the Claude shim does not own the original process, and starting a replacement could create a second writer; Nano does not yet support explicit handover of a terminal-started Claude conversation: %w", req.ThreadID, err)
		}
		if !errors.Is(err, errShimSessionNotFound) && !errors.Is(err, errShimUnreachable) {
			// Once the attach reached the shim, only its answer that it holds
			// no such session proves the child is gone. A timeout, a dropped
			// connection or a bad reply leaves it possibly alive, and --resume
			// would start a second writer.
			return nil, fmt.Errorf("%w: shim attach failed and the child may still be alive: %v", errClaudeRecoveryUnproven, err)
		}
		log.Printf("claude thread %s: shim attach failed (%v) — falling back to %s", req.ThreadID, err, claudeResumeFlag)
	} else {
		if req.ClaudeAnchor.provisional() {
			return nil, fmt.Errorf("claude thread %s was never prompted and can only be re-attached via the shim, which is disabled", req.ThreadID)
		}
		if req.externalResumePosture {
			return nil, fmt.Errorf("cannot establish exclusive Claude attachment for external conversation %s: the Claude shim is disabled, and starting a replacement could create a second writer; Nano does not yet support explicit handover of a terminal-started Claude conversation", req.ThreadID)
		}
	}
	// Only this rung reads the thread's transcript out of a path derived from
	// its cwd, so only this rung needs the project to still be there. Checking
	// any earlier would also refuse rung 1, which needs no cwd at all and is the
	// only rung that can keep a mid-flight turn alive.
	if err := requireResumeCwd(req); err != nil {
		return nil, err
	}
	posture.Args = claudeResumeArgs(posture.Args, req.ThreadID)
	// Only this rung spawns from argv, so only this rung applies canonical
	// desired intent (a shim attach above re-joins the live child as-is).
	posture.Args = applyClaudeResumeOverlay(posture.Args, sess.metadata, sess.hostedThreadID)
	return openClaudeSession(sess, req.ClaudeShimWS, posture, req.ThreadID, sess.snapshotProviderCausation(claudeOutboundFacts(posture.Args)))
}

// errClaudeProvisionalGone: a never-prompted thread's shim child is gone, and a
// thread with no transcript has no other rung to come back on. The conversation
// held nothing, so nothing was lost — spawnSession retires the record on this
// error rather than leaving a detached row that can never resume
// (beginProviderSpawn).
var errClaudeProvisionalGone = errors.New("claude session was never prompted and its hosted child is gone")

// adoptResolvedClaudeNative records a provisional handle's proven native at the
// claim owner and then resumes as it: the session identity, and the attachment
// record re-keyed to the native with the same handle and anchor. The record is
// written under the native before the handle's record is forgotten, so a
// failed write leaves the handle's record on disk. A claim error, including
// one reported after its commit, stops the resume with the record intact; a
// retry finds the recorded resolution.
func (sess *session) adoptResolvedClaudeNative(handle, native string, anchor attachmentRecord) (attachmentRecord, error) {
	if sess.metadata == nil || sess.hostedThreadID == "" {
		return anchor, fmt.Errorf("%w: no claim owner for %s", errClaudeRecoveryUnproven, handle)
	}
	pair := nativeResolution{Handle: handle, Native: native}
	if err := sess.metadata.claimNativeIDResolving(sess.hostedThreadID, sess.provider, native, false, &pair); err != nil {
		return anchor, fmt.Errorf("%w: record provisional %s as %s: %v", errClaudeRecoveryUnproven, handle, native, err)
	}
	sess.stateMu.Lock()
	sess.threadID = native
	sess.stateMu.Unlock()
	sess.setProjectionThreadID(native)
	rec := anchor
	rec.NativeThreadID = native
	if sess.rememberAttachment != nil {
		sess.rememberDurableAttachment(rec)
		if sess.forgetAttachment != nil {
			sess.forgetAttachment(sess.provider, handle)
		}
	}
	return rec, nil
}

// provisionalMayHaveDelivered reports whether any prompt of this hosted thread
// under the provisional handle was, or may have been, handed to the provider.
// Only a coherent pair proves a prompt never was: its outbox row and its own
// journal row both queued, at the same managed revision, with no binding, turn
// or confirmation. Submit writes exactly that pair, and Requeue restores it
// only after a definite refusal. Anything else (another state or journal
// status, a legacy or unmanaged row, an orphan journal row, another owner)
// counts as possibly delivered, and an unreadable store is an error.
func (sess *session) provisionalMayHaveDelivered(ctx context.Context, handle string) (bool, error) {
	if sess.turns == nil {
		return false, errors.New("prompt store unavailable")
	}
	records, err := sess.turns.List(ctx, sess.hostedThreadID)
	if err != nil {
		return false, err
	}
	attempts, err := sess.turns.ListThreadAttempts(ctx, transcriptmodel.ProviderClaude, handle)
	if err != nil {
		return false, err
	}
	journal := make(map[string]prompts.AttemptStatus, len(attempts))
	for _, attempt := range attempts {
		journal[attempt.Attempt.ActionID] = attempt
	}
	paired := 0
	for _, record := range records {
		if record.NativeThreadID != handle {
			continue
		}
		attempt, ok := journal[record.Outbox.ActionID]
		// Both sides must say the same never-handed-over thing: no claim,
		// binding or provider turn on the outbox row, none on its journal.
		outboxQueued := record.Outbox.State == prompts.Queued && record.Revision != 0 && record.Provider == transcriptmodel.ProviderClaude &&
			record.Outbox.Binding == nil && record.ProviderTurnID == "" && record.ClaimToken == ""
		journalQueued := ok && attempt.OutboxState == prompts.Queued && attempt.Attempt.State == prompts.JournalQueued &&
			attempt.Attempt.Revision == record.Revision && attempt.Attempt.HostedThreadID == sess.hostedThreadID &&
			attempt.Attempt.Provider == transcriptmodel.ProviderClaude && attempt.Attempt.Binding == nil &&
			attempt.Attempt.ProviderTurnID == "" && attempt.Attempt.ClaimToken == "" && !attempt.Attempt.Confirmed
		if !outboxQueued || !journalQueued {
			return true, nil
		}
		paired++
	}
	// A journal row this hosted thread's outbox does not hold is another
	// owner's or a legacy row: never proof of an unprompted thread.
	return paired != len(attempts), nil
}

// requireResumeCwd refuses a respawn into a project directory that is gone,
// naming the path while it is still in hand: without the check the child spawns,
// finds no transcript under the path derived from its cwd, and dies with a raw
// error the operator has to decode.
func requireResumeCwd(req spawnRequest) error {
	if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
		return fmt.Errorf("cannot resume claude thread %s: its working directory %s is gone — the project was moved or deleted", req.ThreadID, req.Cwd)
	}
	return nil
}

// openClaudeSession starts a child (openClaudeTransport picks how) and hands
// the wired-up session back. supersedes names the native session id this child
// replaces (a `--resume` target), empty for a fresh start.
func openClaudeSession(sess *session, shimURL string, posture claudeSpawnPosture, supersedes string, causation providerCausationSnapshot) (*claudeSession, error) {
	sess.beginTurnRestore()
	defer sess.cancelTurnRestore()
	c := newClaudeSession(sess, supersedes, causation)
	tr, err := openClaudeTransport(c, shimURL, posture)
	if err != nil {
		return nil, err
	}
	c.tr = tr
	// Before markTransportReady: the composer unlocks on ready, and the first
	// turn's init must find the provisional identity already in place.
	c.adoptDurableIdentity(tr)
	if !c.markTransportReady() {
		// The session was closed while the child was starting. It is ours to
		// clean up — nothing else knows this transport exists.
		_ = tr.close()
		return nil, errSpawnSessionClosed
	}
	// A fresh child needs no init frame, but any output that raced transport
	// setup must settle with readiness instead of being dropped while starting.
	sess.stateMu.Lock()
	epoch, sequence := sess.lifecycleState.Epoch, sess.nextEvidenceSequenceLocked()
	active, turnID, anchor := sess.turnRestoreActive, sess.turnRestoreID, sess.turnRestoreStartedAtMs
	ended := false
	select {
	case <-c.exited:
		ended = true
	default:
	}
	sess.stateMu.Unlock()
	if !ended {
		sess.settleProviderEvidence(claudeLifecycleAdapter{epoch: epoch, sequence: sequence, state: "running", active: active, settled: true}, turnID, anchor)
	}
	go c.startControlChannel()
	return c, nil
}

// adoptDurableIdentity makes a shim-hosted session survive a nano restart from
// the moment it exists, not from its first prompt. A `claude -p` child emits no
// system.init until it has been sent a turn, so a thread the operator started
// but has not prompted yet has no native id — and before this existed, no
// attachment record either: every deploy's restart silently dropped the session
// while its child lived on orphaned in the shim, and a `--resume` respawn in
// the same window was re-spawned on every boot, leaking one child per restart.
//
// The shim session id fills the gap, the same identity-at-spawn idiom the
// resume rung uses (resumeClaudeSession):
//
//   - fresh start: the shim id becomes the session's provisional thread
//     identity — threadID, projection, and an attachment record keyed by it —
//     and supersedes, so the real init retires all of it through the existing
//     succession machinery (captureInit).
//   - `--resume` respawn (supersedes already names the predecessor): the
//     predecessor's record learns which shim child now hosts the thread, so a
//     restart before the first turn re-attaches instead of respawning again.
//
// A direct child reports no durable attachment (it dies with nano), so this is
// a no-op for it — nothing durable exists to identify.
func (c *claudeSession) adoptDurableIdentity(tr claudeTransport) {
	rec, ok := tr.durableAttachment("", "")
	if !ok || rec.ShimSessionID == "" {
		return
	}
	if c.supersedes != "" {
		if c.sess.updateShimSession != nil {
			c.sess.updateShimSession(c.sess.provider, c.supersedes, rec.ShimSessionID)
		}
		return
	}
	c.sess.stateMu.Lock()
	if c.sess.closed || c.sess.threadID != "" {
		// The child spoke before spawn finished wiring — captureInit already
		// landed the real identity — or the operator let the session go.
		// Either way there is nothing provisional to adopt.
		c.sess.stateMu.Unlock()
		return
	}
	// supersedes is written under stateMu so captureInit, which takes the lock
	// before deriving identity, cannot observe the provisional threadID without
	// also observing the supersedes that retires it.
	c.supersedes = rec.ShimSessionID
	c.sess.threadID = rec.ShimSessionID
	c.sess.stateMu.Unlock()
	c.sess.setProjectionThreadID(rec.ShimSessionID)
	c.sess.publishProjectionRuntime()
	c.sess.notifyRuntime()
	c.sess.updateHostedThread("ready", "")
	if c.sess.rememberAttachment == nil {
		return
	}
	rec.NativeThreadID = rec.ShimSessionID
	c.sess.rememberDurableAttachment(rec)
	// Reconcile: a close (DELETE forgets by the threadID it could see) or the
	// real init (captureInit forgets by supersedes) can race the writes above,
	// making the provisional identity stale the moment it hits disk. Re-check
	// and retire it ourselves so neither race leaves a record that would
	// resurrect a closed session — or a duplicate of a real one — on boot.
	c.sess.stateMu.Lock()
	closed, current := c.sess.closed, c.sess.threadID
	c.sess.stateMu.Unlock()
	if closed || current != rec.ShimSessionID {
		if c.sess.forgetAttachment != nil {
			c.sess.forgetAttachment(c.sess.provider, rec.ShimSessionID)
		}
		if !closed && current != "" {
			c.sess.setProjectionThreadID(current) // re-assert the id that won
		}
	}
}

// openClaudeTransport picks the transport: the shim daemon, whose children
// survive a nano restart, falling back to a direct child when it cannot be
// reached.
func openClaudeTransport(c *claudeSession, shimURL string, posture claudeSpawnPosture) (claudeTransport, error) {
	if shimURL = strings.TrimSpace(shimURL); shimURL != "" {
		tr, err := startClaudeShimSession(c, shimURL, posture)
		if err == nil {
			return tr, nil
		}
		log.Printf("WARNING: claude shim unreachable — direct child spawn (no durability): %v", err)
	}
	// Spelled out rather than returned straight through: a typed nil pointer in
	// an interface is not a nil interface, and this one would be checked.
	tr, err := startClaudeChild(c, posture)
	if err != nil {
		return nil, err
	}
	return tr, nil
}

// errSpawnSessionClosed reports a provider handshake that landed after its
// session was already closed. It is not a spawn failure: nobody is waiting for
// this session, and surfacing it through failSpawn would put an error on a
// session the operator deliberately let go of.
var errSpawnSessionClosed = errors.New("session closed while its transport was starting")

// newClaudeSession builds the session shell every transport is wired into. The
// control correlator exists from this moment so a control_response arriving
// while the transport is still being wired up has somewhere to land.
func newClaudeSession(sess *session, supersedes string, causation ...providerCausationSnapshot) *claudeSession {
	var snapshot providerCausationSnapshot
	if len(causation) > 0 {
		snapshot = causation[0]
	}
	return &claudeSession{
		sess:          sess,
		initCausation: snapshot,
		exited:        make(chan struct{}),
		supersedes:    supersedes,
		control:       newClaudeControl(),
	}
}

// claudeOutboundFacts reads only flags that the child will actually receive.
// It lets system.init settle the exact launch/overlay request instead of a
// desired row that happened to be pending later.
func claudeOutboundFacts(args []string) map[string]string {
	facts := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case claudeModelFlag:
			facts[postureFieldModel] = args[i+1]
		case "--permission-mode":
			facts[postureFieldPermissionMode] = args[i+1]
		}
	}
	return facts
}

// markTransportReady publishes the provider handle and promotes the session out
// of "starting" as soon as its transport is up: the child (or the shim session
// it attached to) is streaming and will accept input. Claude's handshake cannot
// gate this — CLI 2.1.220 emits no system.init until it has been *sent* a turn,
// while the composer stays locked for as long as a session reads "starting", so
// waiting for init deadlocks every hosted claude session
// (docs/decisions/claude-ready-on-transport.md). "starting" keeps its meaning:
// transport setup is still in progress.
//
// Handle and status go up together, under one lock and in that order: a fresh
// start wires its transport up in the background now, and the composer unlocks
// on "ready", so a session that read ready with nothing behind it would be a
// session the operator could type into and nothing would receive.
//
// It reports false only for a session the operator closed while the transport
// was coming up: nothing holds a reference to that transport but the caller,
// which must close it, or the child outlives the session that asked for it. A
// session that merely died — a spawn failure translated by captureSpawnFailure,
// or a transport that dropped during attach replay — still adopts its handle
// and is left where it is, so its teardown path still has something to close.
func (c *claudeSession) markTransportReady() bool {
	c.sess.stateMu.Lock()
	if c.sess.closed {
		c.sess.stateMu.Unlock()
		return false
	}
	c.sess.claude = c
	select {
	case <-c.exited:
		c.sess.stateMu.Unlock()
		return true
	default:
	}
	event := c.sess.readyLifecycleEventLocked()
	if c.sess.turnRestoreInProgress {
		event = lifecycle.EventReadyBeforeReplaySettled
	}
	changed, drain := c.sess.transitionLifecycleLocked(event)
	status := c.sess.connectionStatusLocked()
	c.sess.stateMu.Unlock()
	if changed {
		c.sess.publishProjectionRuntime()
		c.sess.notifyRuntime()
		c.sess.updateHostedThread(status, "")
	}
	if drain {
		c.sess.kickQueuedTurnDrain()
	}
	return true
}

// deliverLine routes one line from the child into the session exactly as the
// direct stdout reader always has — same recordWire call, same state probe —
// whichever transport carried it. The transport is passed in rather than read
// from c.tr: lines can arrive while start is still wiring the transport up.
//
// seq is the shim's sequence number for this line, 0 for a direct child.
//
// at is when the line was first seen — the shim's spooled time on a replay,
// zero for a direct child (and for a shim too old to send one), which dates
// the line to now. Without it a replay would stamp the whole conversation with
// the re-attach instant; see session.recordWireAt.
func (c *claudeSession) deliverLine(tr claudeTransport, dir protocol.Direction, line []byte, seq int64, at time.Time) {
	c.deliverFrame(tr, dir, line, seq, at, claudeFrameTurn{}, nil)
}

// deliverFrame applies one frame, or holds it behind a send in flight. then,
// if set, runs once the frame is applied, held or not.
func (c *claudeSession) deliverFrame(tr claudeTransport, dir protocol.Direction, line []byte, seq int64, at time.Time, turn claudeFrameTurn, then func()) {
	replaying := tr.isReplaying()
	c.turnIngressMu.Lock()
	if c.turnIngressHeld {
		c.turnIngressPending = append(c.turnIngressPending, claudeIngressLine{
			tr: tr, dir: dir, line: append([]byte(nil), line...), seq: seq, at: at, replaying: replaying, turn: turn, then: then,
		})
		c.turnIngressMu.Unlock()
		return
	}
	c.deliverLineLocked(tr, dir, line, seq, at, replaying, turn)
	c.turnIngressMu.Unlock()
	if then != nil {
		then()
	}
}

// noteAnswered drops the prompts a result names in user_message_uuids, which
// lists every prompt its execution took, merged or absorbed mid-turn. It
// reports whether a prompt Nano wrote is still unanswered.
func (c *claudeSession) noteAnswered(result []byte) bool {
	var named struct {
		UserMessageUUIDs []string `json:"user_message_uuids"`
	}
	_ = json.Unmarshal(result, &named)
	c.unansweredMu.Lock()
	defer c.unansweredMu.Unlock()
	for _, id := range named.UserMessageUUIDs {
		delete(c.unanswered, id)
	}
	return len(c.unanswered) > 0
}

// deliverHeldLocked applies one held frame and what follows it.
func (c *claudeSession) deliverHeldLocked(pending claudeIngressLine) {
	c.deliverLineLocked(pending.tr, pending.dir, pending.line, pending.seq, pending.at, pending.replaying, pending.turn)
	if pending.then != nil {
		pending.then()
	}
}

func (c *claudeSession) deliverLineLocked(tr claudeTransport, dir protocol.Direction, line []byte, seq int64, at time.Time, replaying bool, turn claudeFrameTurn) {
	if at.IsZero() {
		at = time.Now()
	}
	if !replaying {
		if anchor := c.sess.turnAnchor(); anchor > 0 && at.UnixMilli() < anchor {
			at = time.UnixMilli(anchor)
		}
		if !c.lastLiveWireAt.IsZero() && at.Before(c.lastLiveWireAt) {
			at = c.lastLiveWireAt
		}
		c.lastLiveWireAt = at
	}
	// The init line is the first native-thread fact on a fresh Claude turn.
	// Name the projection's thread from that same line before parsing it; the
	// broader session/attachment state still changes in captureState below.
	result := false
	if dir == protocol.Recv {
		var init struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(line, &init) == nil && init.Type == "system" && init.Subtype == "init" && init.SessionID != "" {
			c.sess.setProjectionThreadID(init.SessionID)
		}
		result = init.Type == "result"
	}
	// Both run on the session loop, where the frame is applied.
	var gate func() error
	gated, _ := tr.(claudeQueuedWorkGate)
	if gated != nil {
		gate = gated.queuedWorkAllowed
	}
	c.sess.recordWireWith(gate, func() {
		if gated != nil {
			gated.admitted()
		}
		switch {
		case c.sess.projection == nil:
		case result:
			// On every transport a result's turn is the prompt it names, never
			// a frame tag or the prompt last sent.
			id, named := c.sess.claudeAnsweredTurn(line)
			c.sess.projection.setClaudeResultTurnID(id)
			native := c.sess.currentThreadID()
			if c.noteAnswered(line) && !named && c.unnamedLoggedFor != native {
				c.unnamedLoggedFor = native
				log.Printf("Claude result on %s names no prompt while one Nano wrote is unanswered; it finishes no operation (logged once per transcript)", c.sess.hostedThreadID)
			}
		case turn.known:
			c.sess.projection.setClaudeFrameTurnID(turn.id)
		}
	}, dir, line, !replaying, replaying, at, seq)
	if dir == protocol.Recv {
		c.captureState(tr, line)
	} else if replaying {
		// Send lines only ever arrive during a replay, and one of them matters: a
		// previous nano's answer to a control request the CLI raised. It is the
		// evidence that stops that request being recovered as if it were still
		// open (claude_control.go, replayedControl).
		c.captureReplayedSend(line)
	}
	if replaying {
		c.noteReplayedTurnBoundary(dir, line, seq)
		return
	}
	// After captureState, not before: the init line is where the native id
	// arrives, and the anchor cannot be written to a record that has no identity
	// yet. A claude session is sent its first turn BEFORE it has ever announced
	// an id (CLI 2.1.220 emits no init until then), so this ordering is the
	// difference between the first turn's anchor being persisted and being lost.
	c.syncTurnAnchor()
}

func (c *claudeSession) releaseTurnIngress() {
	c.turnIngressMu.Lock()
	c.releaseTurnIngressLocked()
	c.turnIngressMu.Unlock()
}

func (c *claudeSession) turnIngressActive() bool {
	c.turnIngressMu.Lock()
	defer c.turnIngressMu.Unlock()
	return c.turnIngressHeld
}

func (c *claudeSession) releaseTurnIngressLocked() {
	if !c.turnIngressHeld {
		return
	}
	for _, pending := range c.turnIngressPending {
		c.deliverHeldLocked(pending)
	}
	c.turnIngressPending = nil
	c.turnIngressHeld = false
}

func (c *claudeSession) drainTurnIngressThroughInitLocked() {
	through := -1
	for index, pending := range c.turnIngressPending {
		if pending.dir != protocol.Recv {
			continue
		}
		var probe struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(pending.line, &probe) == nil && probe.Type == "system" && probe.Subtype == "init" {
			through = index
			break
		}
	}
	if through < 0 {
		return
	}
	for _, pending := range c.turnIngressPending[:through+1] {
		c.deliverHeldLocked(pending)
	}
	c.turnIngressPending = append([]claudeIngressLine(nil), c.turnIngressPending[through+1:]...)
}

// noteReplayedTurnBoundary follows the turn boundaries through a spool replay
// so that, when the replay ends, "is a turn still in flight, and which line
// opened it" is answered from the spool alone. Probed 2026-07-31: the shim
// replays nano's own send lines WITH their sequence numbers, so this is real
// identity evidence and not an inference.
func (c *claudeSession) noteReplayedTurnBoundary(dir protocol.Direction, line []byte, seq int64) {
	active, ok := claudeTurnBoundaryTransition(dir, line)
	if !ok {
		return
	}
	if active {
		c.replayTurnSeq.Store(seq)
	} else {
		c.replayTurnSeq.Store(0)
	}
}

// restoreReplayedTurnAnchor is the claude half of surviving a nano restart
// mid-turn, run once the replay has landed. It restores the anchor ONLY when
// the spool and the stored record agree about the same turn:
//
//   - the replay ended with a turn-opening user line and no result after it
//     (the turn is genuinely still running, not one that finished unattended);
//   - the record carries an anchor at all (an older nano's record has none, and
//     that degrades to no timer, never to replay-arrival time);
//   - that user line's sequence number is greater than the record's anchor seq,
//     which is hard identity by seq (D9/D11) — the record was written before
//     the line was sent, so a record describing an EARLIER turn cannot pass.
//
// Anything else clears the record: a stale anchor left lying around would
// eventually be restored onto the wrong turn.
func (c *claudeSession) restoreReplayedTurnAnchor() {
	turnSeq := c.replayTurnSeq.Load()
	rec := c.storedAnchor
	if turnSeq > 0 && rec.TurnStartedAtMs > 0 && turnSeq > rec.TurnAnchorSeq {
		// The restored turn is the one the record describes, so its anchor seq
		// still stands — carry it, or the next sync would rewrite the record
		// with a zero seq and disarm the very check that just passed.
		c.anchorSeq.Store(rec.TurnAnchorSeq)
		if c.sess.restoreTurnAnchorQuietly(rec.TurnStartedAtMs) {
			log.Printf("claude session %s: restored turn anchor %d (spool seq %d > anchor seq %d)",
				c.sess.id, rec.TurnStartedAtMs, turnSeq, rec.TurnAnchorSeq)
		}
	}
	c.syncTurnAnchor()
}

// syncTurnAnchor keeps the attachment record's anchor equal to the session's.
// Called after every live line, so the fast path is a single atomic compare;
// only a turn boundary actually rewrites the file. A session with no native id
// yet has no record to write to, and one whose transport dies with nano has no
// record at all — setTurnAnchor never creates one.
func (c *claudeSession) syncTurnAnchor() {
	c.anchorMu.Lock()
	defer c.anchorMu.Unlock()
	anchor := c.sess.turnAnchor()
	if c.persistedAnchor.Load() == anchor {
		return
	}
	threadID := c.sess.currentThreadID()
	if threadID == "" || c.sess.updateTurnAnchor == nil {
		// No record to write to yet. Deliberately do NOT mark this anchor as
		// persisted: a claude session is sent its first turn before it has ever
		// announced a native id, so recording "done" here would lose exactly the
		// anchor a restart during that first turn needs.
		return
	}
	seq := int64(0)
	if anchor != 0 {
		seq = c.anchorSeq.Load()
	}
	c.sess.updateTurnAnchor(c.sess.provider, threadID, anchor, seq)
	c.persistedAnchor.Store(anchor)
}

// rekeyTurnAnchor forces the next sync to rewrite the anchor: the record it was
// last persisted to has just been retired along with the provisional identity,
// so "already persisted" no longer means "on the record that survives". -1 can
// equal no real anchor value (0 is no-turn, positive is a start time), so the
// compare in syncTurnAnchor cannot skip the rewrite.
func (c *claudeSession) rekeyTurnAnchor() {
	c.anchorMu.Lock()
	c.persistedAnchor.Store(-1)
	c.anchorMu.Unlock()
	c.syncTurnAnchor()
}

// markExited records the child's death exactly once, whichever transport
// noticed it: stdout draining, a session/exited notification, or the shim
// connection dropping.
func (c *claudeSession) markExited() {
	c.exitOnce.Do(func() {
		close(c.exited)
		c.sess.observeConnection(lifecycle.EventRuntimeExited)
		log.Printf("claude session %s exited", c.sess.id)
	})
}

func (c *claudeSession) Close() error { return c.tr.close() }

// claudeChildTransport is the fallback transport: a `claude -p` child owned by
// this nano process. It dies with nano, so it never remembers an attachment.
type claudeChildTransport struct {
	owner *claudeSession
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex
}

func startClaudeChild(owner *claudeSession, posture claudeSpawnPosture) (*claudeChildTransport, error) {
	cmd := exec.Command(posture.Bin, posture.Args...)
	cmd.Dir = owner.sess.cwd
	cmd.Stderr = os.Stderr
	// Appended, so it overrides an entrypoint nano itself inherited (a nano
	// launched from a Claude session carries `cli`): exec keeps the last
	// occurrence of a duplicated key.
	cmd.Env = append(os.Environ(), claudeEntrypointEnv+"="+claudeNanoEntrypoint)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting claude: %w", err)
	}
	t := &claudeChildTransport{owner: owner, cmd: cmd, stdin: stdin}
	go t.readLoop(stdout)
	return t, nil
}

func (t *claudeChildTransport) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...) // scanner reuses its buffer
		// A direct child streams live, so arrival time is the line's time.
		t.owner.deliverLine(t, protocol.Recv, line, 0, time.Time{})
	}
	if err := scanner.Err(); err != nil {
		log.Printf("claude read loop: %v", err)
	}
	// Stdout is drained; safe to reap. Wait exactly once, here.
	t.cmd.Wait()
	t.owner.markExited()
}

// isReplaying: never. A direct child is spawned fresh, so every line it emits
// is live by construction.
func (t *claudeChildTransport) isReplaying() bool { return false }

// lastSeq: none. A direct child's stdout is not sequenced, and it dies with
// nano anyway, so there is no restart for an anchor seq to survive.
func (t *claudeChildTransport) lastSeq() int64 { return 0 }

func (t *claudeChildTransport) sendLine(line []byte) error {
	return t.sendLineContext(context.Background(), line)
}

func (t *claudeChildTransport) sendLineContext(ctx context.Context, line []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		if file, ok := t.stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
			if err := file.SetWriteDeadline(deadline); err != nil {
				return err
			}
			defer file.SetWriteDeadline(time.Time{})
		}
	}
	_, err := t.stdin.Write(append(line, '\n'))
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// close ends the session: closing stdin asks `claude -p` to exit cleanly
// (letting it flush the native transcript); kill if it lingers.
func (t *claudeChildTransport) close() error {
	t.writeMu.Lock()
	t.stdin.Close()
	t.writeMu.Unlock()
	select {
	case <-t.owner.exited:
	case <-time.After(claudeCloseGrace):
		t.cmd.Process.Kill()
		<-t.owner.exited
	}
	return nil
}

// durableAttachment: never. A direct child is killed by the restart that would
// make the record useful, so remembering it would offer a re-attach that
// cannot work.
func (t *claudeChildTransport) durableAttachment(string, string) (attachmentRecord, bool) {
	return attachmentRecord{}, false
}

// claudeMissingTranscriptMarker is how `claude -p --resume` reports a session
// whose native transcript is not on disk. Probed 2026-07-29 under a scratch
// HOME: the CLI prints this sentence on stderr, emits one stream-json result line
// carrying it in `errors`, and exits 1 without ever announcing a system.init.
const claudeMissingTranscriptMarker = "No conversation found with session ID"

// captureState derives session state from one line the child sent: the
// system.init handshake, or a fatal result line arriving in its place. One
// parse serves both — every received line passes through here.
func (c *claudeSession) captureState(tr claudeTransport, line []byte) {
	var probe struct {
		Type           string   `json:"type"`
		Subtype        string   `json:"subtype"`
		SessionID      string   `json:"session_id"`
		Model          string   `json:"model"`
		PermissionMode string   `json:"permissionMode"`
		IsError        bool     `json:"is_error"`
		Errors         []string `json:"errors"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return
	}
	switch {
	case probe.Type == "system" && probe.Subtype == "init":
		c.captureInit(tr, line, probe.SessionID, probe.Model, probe.PermissionMode)
	case probe.Type == "result" && probe.IsError:
		c.captureSpawnFailure(probe.SessionID, probe.Errors)
	case probe.Type == claudeControlResponseType:
		c.applyControlResponse(tr, line)
	case probe.Type == claudeControlRequestType:
		c.handleInboundControl(tr, line)
	}
}

// captureInit snapshots the system.init line — the Claude analog of the
// thread/start response (model, tools, mcp_servers, permission mode).
func (c *claudeSession) captureInit(tr claudeTransport, line []byte, sessionID, model, permissionMode string) {
	c.sess.stateMu.Lock()
	closed := c.sess.closed
	c.sess.stateMu.Unlock()
	// This child's own init resolves the provisional handle it ran under. The
	// claim owner records that pair before anything else adopts the identity,
	// so nothing can retire the handle without it; if it cannot be recorded,
	// the identity stays provisional and a replayed or later init retries.
	if !closed && c.supersedes != "" && c.supersedes != sessionID && c.sess.metadata != nil && c.sess.hostedThreadID != "" {
		if rec, ok := tr.durableAttachment(sessionID, model); ok && rec.ShimSessionID == c.supersedes {
			pair := nativeResolution{Handle: c.supersedes, Native: sessionID}
			if err := c.sess.metadata.claimNativeIDResolving(c.sess.hostedThreadID, c.sess.provider, sessionID, false, &pair); err != nil {
				log.Printf("claude %s: record provisional %s as %s: %v", c.sess.hostedThreadID, c.supersedes, sessionID, err)
				// The child did complete its handshake: a later error result
				// must not be read as a spawn that never started. The session
				// keeps its provisional identity, so nothing resolves through
				// this child until the pair is recorded.
				c.initSeen.Store(true)
				return
			}
		}
	}
	c.sess.stateMu.Lock()
	if c.sess.closed {
		// The operator let this session go while its child was still coming up.
		// An init landing now must not bring it back — not as a ready session,
		// and not as a durable attachment offering to re-attach to a session
		// that was deliberately closed. The line itself still reaches the wire
		// buffer; only the identity it carries is refused.
		c.sess.stateMu.Unlock()
		return
	}
	c.sess.threadID = sessionID
	c.sess.model = model
	c.sess.stateMu.Unlock()
	c.initSeen.Store(true)
	c.sess.setState("init", json.RawMessage(line))
	// Live inits are provider evidence of what the child actually runs. A shim
	// replay re-delivers the ORIGINAL spawn's init, which the log already
	// holds — re-appending it would postdate stale evidence past any operator
	// decision (set_model) taken since, and the fold serves the newest entry.
	freshness := metadataFreshnessLive
	if tr.isReplaying() {
		freshness = metadataFreshnessReplayed
	}
	values := map[string]string{postureFieldModel: model, postureFieldPermissionMode: permissionMode}
	cause := c.initCausation
	if tr.isReplaying() {
		cause = nil
	}
	c.sess.recordProviderEvidence("claude_system_init", values, freshness, cause)
	c.sess.setProjectionThreadID(sessionID)
	// A fresh claude start learns its native id here (init lands with the
	// first turn), which is the earliest a dialog-supplied name can be written.
	c.sess.applyPendingName(sessionID)
	// The threadID landing is what makes this session matchable by the
	// catalog's runtime overlay (provider + nativeId).
	c.sess.publishProjectionRuntime()
	c.sess.notifyRuntime()
	c.sess.updateHostedThread("ready", "")
	// A `--resume` child mints a fresh native id; an attach lands back on the
	// one it asked for. Only the former is a succession. A provisional identity
	// (the shim handle standing in for a native id on a never-prompted session,
	// adoptDurableIdentity) counts too: this init retires it the same way.
	succeeds := c.supersedes != "" && c.supersedes != sessionID
	provisionalRetired := false
	if succeeds {
		err := c.sess.requireTransactionalTurnsLocked()
		if err != nil {
			log.Printf("rebind transactional turns %s to %s: %v", c.supersedes, sessionID, err)
		} else {
			outbox, err := c.sess.turns.RebindNativeThread(context.Background(), c.sess.hostedThreadID, transcriptmodel.Provider(c.sess.provider), c.supersedes, sessionID)
			if err != nil {
				log.Printf("rebind transactional turns %s to %s: %v", c.supersedes, sessionID, err)
			} else {
				for _, entry := range outbox {
					c.sess.publishTransactionalOutbox(entry)
				}
			}
		}
	}
	// Now that the native id is authoritative, persist the attachment — but
	// only for a transport whose child outlives nano.
	if c.sess.rememberAttachment != nil {
		if rec, ok := tr.durableAttachment(sessionID, model); ok {
			provisionalRetired = succeeds && c.supersedes == rec.ShimSessionID
			if succeeds && !provisionalRetired {
				// Thread-lineage evidence: the one place that knows these two
				// ids are the same conversation. Nothing
				// reads it yet, and nothing should be inferred from its absence
				// on records written before this line existed. A retired
				// provisional identity is excluded — it was this transport's own
				// shim handle, not a predecessor conversation.
				rec.ContinuedFrom = c.supersedes
			}
			c.sess.rememberDurableAttachment(rec)
		}
	}
	// The superseded record now names a session nothing can ever re-attach to:
	// retire it, or it sits in the sidebar as a permanently detached row that
	// forks the conversation again on every click. Only here, on the
	// successor's own init — a resume that never got that far must leave the
	// record intact.
	if succeeds && c.sess.forgetAttachment != nil {
		c.sess.forgetAttachment(c.sess.provider, c.supersedes)
	}
	if provisionalRetired {
		// This init is the first turn's: the turn's anchor was persisted onto
		// the provisional record, which was just forgotten. Rewrite it under the
		// real id, or a restart during the session's very first turn loses the
		// anchor — precisely the case the persistence exists for.
		c.rekeyTurnAnchor()
	}
}

// captureSpawnFailure surfaces a child that died before its handshake as the
// session's spawn error, so a failed resume reads as a sentence instead of an
// empty transcript that silently goes "exited". Pre-init only: after the
// handshake a failing result is one turn's error, not the session's — and
// "pre-init" means initSeen, not an empty thread id, which a `--resume` child
// no longer has (see the field's comment).
func (c *claudeSession) captureSpawnFailure(sessionID string, errs []string) {
	if c.initSeen.Load() {
		return
	}
	c.sess.failSpawn(claudeSpawnFailure(sessionID, errs))
}

// claudeSpawnFailure translates the child's own error text into something the
// operator can act on. Anything unrecognized passes through verbatim rather
// than being flattened into a generic message.
func claudeSpawnFailure(sessionID string, errs []string) string {
	for _, e := range errs {
		if strings.Contains(e, claudeMissingTranscriptMarker) {
			return fmt.Sprintf("cannot resume claude thread %s: its native transcript is no longer on disk, so there is no conversation to restore", sessionID)
		}
	}
	if joined := strings.TrimSpace(strings.Join(errs, "; ")); joined != "" {
		return "claude could not start the session: " + joined
	}
	return "claude exited before the session handshake"
}

func (c *claudeSession) SendTurn(text string, contexts ...context.Context) error {
	_, err := c.sendTurn(text, contexts...)
	return err
}

func (c *claudeSession) sendTurn(text string, contexts ...context.Context) (string, error) {
	return c.sendTurnWithEventID(text, "", contexts...)
}

func (c *claudeSession) sendTurnWithEventID(text, eventID string, contexts ...context.Context) (string, error) {
	msg := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}},
	}
	if eventID != "" {
		msg["uuid"] = eventID
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	sentAt := time.Now()
	c.turnIngressMu.Lock()
	if c.turnIngressHeld {
		c.turnIngressMu.Unlock()
		return "", fmt.Errorf("claude turn ingress is already held")
	}
	c.turnIngressHeld = true
	// Sampled before anything is sent: this is the "everything the daemon has
	// already told us" watermark the replay check compares the turn's own line
	// against (see anchorSeq).
	c.anchorSeq.Store(c.tr.lastSeq())
	c.turnIngressMu.Unlock()
	// A local send record is delivery evidence, not intent. Recording it before
	// a failed transport write would falsely confirm the browser's pending echo
	// and open a turn Claude never received. The shim does not echo live sends,
	// so record exactly once, immediately after its input acknowledgement.
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	var sendErr error
	if sender, ok := c.tr.(claudeContextLineSender); ok {
		sendErr = sender.sendLineContext(ctx, line)
	} else {
		sendErr = c.tr.sendLine(line)
	}
	if err := sendErr; err != nil {
		c.releaseTurnIngress()
		return "", err
	}
	c.turnIngressMu.Lock()
	// A fresh Claude child reports its native id in system.init. Process only
	// that ordered prefix before recording Nano's outbound user line; later
	// provider output remains held until the accepted binding commits.
	c.drainTurnIngressThroughInitLocked()
	c.deliverLineLocked(c.tr, protocol.Send, line, 0, sentAt, false, claudeFrameTurn{})
	if eventID != "" {
		c.unansweredMu.Lock()
		if c.unanswered == nil {
			c.unanswered = map[string]bool{}
		}
		c.unanswered[eventID] = true
		c.unansweredMu.Unlock()
	}
	turnID := ""
	if c.sess.projection != nil {
		turnID = c.sess.projection.currentTurnID()
	}
	c.releaseTurnIngressLocked()
	c.turnIngressMu.Unlock()
	return turnID, nil
}

var claudeSlugRE = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeTranscriptPath computes where the CLI persists this session's native
// JSONL (verified pattern: cwd with non-alphanumerics mapped to '-').
func claudeTranscriptPath(cwd, sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil || sessionID == "" {
		return ""
	}
	slug := claudeSlugRE.ReplaceAllString(cwd, "-")
	return filepath.Join(home, ".claude", "projects", slug, sessionID+".jsonl")
}
