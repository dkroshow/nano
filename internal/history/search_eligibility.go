package history

// ConversationSearchIdentity is the stable source identity a future derived
// search document must retain. Projection membership is intentional: an item
// from a superseded projection must never be confused with the active item.
type ConversationSearchIdentity struct {
	ProjectionID  int64
	ProjectionKey string
	ThreadID      string
	ItemID        string
}

// ConversationSearchCandidate is provider-neutral normalized transcript
// evidence. Callers provide body presence explicitly: nil means there is no
// body row, while a pointer to "" means the retained body is canonically empty.
type ConversationSearchCandidate struct {
	Identity    ConversationSearchIdentity
	Active      bool
	ThreadKind  ThreadKind
	Type        TranscriptItemType
	Role        TranscriptRole
	Synthetic   bool
	DisplayText string
	BodyText    *string
}

// EligibleConversationSearchItem is the single corpus boundary for permanent
// conversation search. It intentionally contains no provider prompt or origin
// heuristics: adapters normalize those concerns before transcript persistence.
type EligibleConversationSearchItem struct {
	Identity ConversationSearchIdentity
	Role     TranscriptRole
	Text     string
}

// CanonicalRetainedText returns the complete Nano-retained value for one item.
// A present body is authoritative even when it is empty; display text is an
// inline preview in that case and must not be concatenated or substituted.
func CanonicalRetainedText(displayText string, bodyText *string) string {
	if bodyText != nil {
		return *bodyText
	}
	return displayText
}

// EligibleConversationItem returns one canonical searchable conversation item
// or false when the normalized evidence is outside the permanent corpus.
func EligibleConversationItem(candidate ConversationSearchCandidate) (EligibleConversationSearchItem, bool) {
	if !eligibleConversationSearchMetadata(candidate) {
		return EligibleConversationSearchItem{}, false
	}
	return EligibleConversationSearchItem{
		Identity: candidate.Identity,
		Role:     candidate.Role,
		Text:     CanonicalRetainedText(candidate.DisplayText, candidate.BodyText),
	}, true
}

// eligibleConversationSearchMetadata is the complete text-independent corpus
// policy. Identity-only callers use it directly; if a future corpus rule needs
// retained text, that change must deliberately extend both the text-bearing and
// identity-only paths rather than silently evaluating incomplete evidence.
func eligibleConversationSearchMetadata(candidate ConversationSearchCandidate) bool {
	return candidate.Active && searchEligibleThreadKind(candidate.ThreadKind) && searchEligibleTypeAndRole(candidate)
}

func searchEligibleThreadKind(kind ThreadKind) bool {
	return kind == ThreadMain || kind == ThreadSubagent || kind == ThreadFork
}

func searchEligibleTypeAndRole(candidate ConversationSearchCandidate) bool {
	switch candidate.Type {
	case ItemUserMessage:
		return candidate.Role == RoleUser
	case ItemAgentMessage:
		return candidate.Role == RoleAssistant
	case ItemInteraction:
		return !candidate.Synthetic && (candidate.Role == RoleUser || candidate.Role == RoleAssistant)
	default:
		return false
	}
}
