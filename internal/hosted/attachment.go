package hosted

// attachmentProviderDefault is the provider assumed for records written before
// attachments were provider-aware: the store was codex-only by construction, so
// a missing provider can only mean codex.
const attachmentProviderDefault = "codex"

// Attachment is one durable transport attachment. Cwd, Model, and tier
// remain only so old files can be imported; live writes always scrub them.
//
// Bin and Args are claude-only: the resolved binary and the exact spawn args of
// the original child, stored so a `--resume` fallback can restore the session's
// posture flags (permission mode, allowed tools, model) instead of silently
// downgrading it. Codex records never set them — a codex resume gets its
// posture from the app-server's own thread state.
//
// ContinuedFrom names the native id this session succeeded, set only when a
// resume minted a new one (claude's `--resume` rung). It is provider-generic
// and write-only for now: the one durable trace that two native ids are the
// same conversation, kept so a later thread-lineage view can join them.
// Nothing reads it yet, so its absence on a record proves nothing.
// TurnStartedAtMs / TurnAnchorSeq are the claude live-turn anchor, persisted
// here because it is the only place nano can put it: the shim's spool stamps
// its own timestamps but never sends them, and its JSON-RPC surface is a frozen
// contract. Both are written when a turn opens and cleared when it closes, so a
// record carrying them describes a turn that was in flight when nano last
// looked. TurnAnchorSeq is the shim sequence number nano had already seen when
// it sent the turn's user line — sampled BEFORE the send, so every line the
// shim numbers from then on, the user line included, is strictly greater. That
// makes "the replayed user line's seq is greater than TurnAnchorSeq" hard
// identity evidence (D9/D11) rather than an assumption. A record written by an
// older nano simply has neither field, which degrades to no anchor — never to
// replay-arrival time.
// ShimSessionID is claude-only: the shim daemon's own handle for the child
// hosting this thread. It is what boot re-attach attaches by when the native
// id cannot answer — a child that has never been sent a turn announces no
// system.init, so its native id is unknown to nano AND to the shim's own
// byNativeID index. A record whose NativeThreadID equals its ShimSessionID is
// provisional (see Provisional): the thread has no native identity yet, and
// the shim child is the only thing the record can name.
type Attachment struct {
	Provider              string `json:"provider"`
	NativeThreadID        string `json:"nativeThreadId"`
	HostedThreadID        string `json:"hostedThreadId,omitempty"`
	InvokerHostedThreadID string `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID string `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider       string `json:"invokerProvider,omitempty"`
	LegacyAgentSessionID  string `json:"agentSessionId,omitempty"`
	PresentationMode      string `json:"presentationMode,omitempty"`
	Cwd                   string `json:"cwd,omitempty"`
	Model                 string `json:"model,omitempty"`
	// PendingName exists only while a Claude child still has a provisional
	// shim identity. The real native id has not landed yet, so the ordinary
	// thread-name store has no durable key to write; this carries the operator's
	// New Window title across a Nano restart until captureInit can apply it.
	PendingName     string   `json:"pendingName,omitempty"`
	AttachedAt      string   `json:"attachedAt"` // RFC3339
	Bin             string   `json:"bin,omitempty"`
	Args            []string `json:"args,omitempty"`
	ShimSessionID   string   `json:"shimSessionId,omitempty"`
	ContinuedFrom   string   `json:"continuedFrom,omitempty"`
	TurnStartedAtMs int64    `json:"turnStartedAtMs,omitempty"`
	TurnAnchorSeq   int64    `json:"turnAnchorSeq,omitempty"`
	TurnServiceTier string   `json:"turnServiceTier,omitempty"`
}

// Provisional reports a record for a thread that was started but never
// prompted: no system.init has landed, so the shim session id is standing in
// for the native id. Such a thread has no transcript on disk — the `--resume`
// fallback rung does not exist for it, and losing the shim child loses the
// (empty) conversation for good.
func (r Attachment) Provisional() bool {
	return r.ShimSessionID != "" && r.NativeThreadID == r.ShimSessionID
}

// AttachmentKey is a record's identity: the native thread id survives a restart
// (the nano session id does not), but providers mint their ids independently,
// so the pair — not the id alone — addresses one attachment.
type AttachmentKey struct {
	// Provider scopes native identifiers that different providers may reuse.
	Provider string `json:"-"`
	// NativeThreadID is the provider-owned identity retained across restarts.
	NativeThreadID string `json:"-"`
}

// Normalized fills in the provider a legacy record lacks, so every stored
// record is addressable and every rewrite of the file carries the field.
func (r Attachment) Normalized() Attachment {
	if r.Provider == "" {
		r.Provider = attachmentProviderDefault
	}
	if r.HostedThreadID == "" {
		r.HostedThreadID = r.LegacyAgentSessionID
	}
	r.LegacyAgentSessionID = ""
	return r
}

// Key returns the provider-scoped durable native identity.
func (r Attachment) Key() AttachmentKey {
	return AttachmentKey{Provider: r.Provider, NativeThreadID: r.NativeThreadID}
}
