package threadindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
)

const (
	// DefaultMessagesTail is the conversational tail length when none is requested.
	DefaultMessagesTail = 20
	// MaxMessagesTail rides the transcript page limit: the tail is
	// assembled from transcript pages, so it cannot ask for more per page.
	MaxMessagesTail = MaxThreadTranscriptLimit
	// maxThreadMessagePages bounds the walk backwards. A page can be mostly
	// non-message items, so one page does not always fill the tail; three is
	// generous without turning a tail request into a transcript scan.
	maxThreadMessagePages = 3
	// maxThreadReferenceCandidates bounds the list an ambiguous reference
	// reports back.

	// MessagesFormatText selects the plain-text conversational tail.
	MessagesFormatText = "text"
	// MessagesFormatJSON selects the structured conversational tail.
	MessagesFormatJSON = "json"

	// MessageRoleUser is a user-authored conversational message.
	MessageRoleUser = "user"
	// MessageRoleAgent is an agent-authored conversational message.
	MessageRoleAgent = "agent"
	// threadMessagesResponseReserve covers the small top-level object, commas,
	// and count digits while per-message accounting uses actual JSON bytes.
	threadMessagesResponseReserve = 512
)

// threadMessagesHiddenCategories hides everything that is not message-shaped,
// mirroring the enhanced view's own default so this endpoint and the transcript
// UI agree on what counts as conversation. The role filter then narrows to user
// and agent messages. Matching the default exactly also keeps the backward
// cursor valid across pages.
var threadMessagesHiddenCategories = []string{"reasoning", "toolCall", "toolResult", "system"}

// MessagesQuery selects the bounded conversational tail and output format.
type MessagesQuery struct {
	Tail   int
	Roles  map[string]bool
	Format string
}

// Message is a bounded user or agent row, with explicit retained-body state.
type Message struct {
	Role              string  `json:"role"`
	At                *string `json:"at"`
	Text              string  `json:"text"`
	Complete          bool    `json:"complete"`
	ItemID            string  `json:"itemId,omitempty"`
	ContentRef        string  `json:"contentRef,omitempty"`
	OriginalBytes     int64   `json:"originalBytes,omitempty"`
	RetainedTruncated bool    `json:"retainedTruncated"`
	ContentState      string  `json:"contentState"`
	Preview           string  `json:"-"`
}

// messagesResponse is the agent-facing conversational tail.
type messagesResponse struct {
	ThreadID string    `json:"threadId"`
	Count    int       `json:"count"`
	Messages []Message `json:"messages"`
}

// HandleThreadMessages serves the saved catalog thread messages endpoint.
func (s *Handlers) HandleThreadMessages(w http.ResponseWriter, r *http.Request, rawID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(TranscriptCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	reference, err := url.PathUnescape(rawID)
	if err != nil || strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	query, err := ParseMessagesQuery(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}

	resolver, _ := s.catalog.(ReferenceResolver)
	id, err := ResolveReference(r.Context(), resolver, reference)
	if err != nil {
		WriteThreadReferenceError(w, err)
		return
	}
	messages, err := CollectThreadMessages(r.Context(), catalog, id, query)
	if err != nil {
		WriteTranscriptBackendError(w, err)
		return
	}
	messages = boundThreadMessages(id, messages)
	if query.Format == MessagesFormatJSON {
		WriteCatalogJSON(w, http.StatusOK, messagesResponse{ThreadID: id, Count: len(messages), Messages: messages})
		return
	}
	for len(messages) > 0 && len(renderThreadMessagesText(messages)) > MaxThreadTranscriptBytes {
		messages = messages[1:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, renderThreadMessagesText(messages))
}

// ParseMessagesQuery validates a conversational-tail query before reference resolution.
func ParseMessagesQuery(r *http.Request) (MessagesQuery, error) {
	q := r.URL.Query()
	query := MessagesQuery{
		Tail:   DefaultMessagesTail,
		Roles:  map[string]bool{MessageRoleUser: true, MessageRoleAgent: true},
		Format: MessagesFormatText,
	}
	if raw := strings.TrimSpace(q.Get("tail")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return query, errors.New("tail must be a positive integer")
		}
		query.Tail = value
	}
	if query.Tail > MaxMessagesTail {
		query.Tail = MaxMessagesTail
	}
	if raw := strings.TrimSpace(q.Get("roles")); raw != "" {
		roles := map[string]bool{}
		for _, value := range strings.Split(raw, ",") {
			switch value = strings.TrimSpace(value); value {
			case MessageRoleUser, MessageRoleAgent:
				roles[value] = true
			case "":
			default:
				return query, fmt.Errorf("unknown role %q; roles are user and agent", value)
			}
		}
		if len(roles) == 0 {
			return query, errors.New("roles must name at least one of user, agent")
		}
		query.Roles = roles
	}
	if raw := strings.TrimSpace(q.Get("format")); raw != "" {
		if raw != MessagesFormatText && raw != MessagesFormatJSON {
			return query, errors.New("format must be text or json")
		}
		query.Format = raw
	}
	return query, nil
}

// CollectThreadMessages walks transcript pages backwards until it has the tail
// the caller asked for, then returns it in chronological order. Walking is
// bounded twice — by page count and by the transcript response budget — so an
// enormous thread costs the same as a small one.
func CollectThreadMessages(ctx context.Context, catalog TranscriptCatalog, id string, query MessagesQuery) ([]Message, error) {
	newestFirst := make([]Message, 0, query.Tail)
	budget := int64(MaxThreadTranscriptBytes - threadMessagesResponseReserve)
	before := ""
	for page := 0; page < maxThreadMessagePages && len(newestFirst) < query.Tail && budget > 0; page++ {
		response, err := catalog.LoadTranscript(ctx, id, TranscriptQuery{
			Limit: query.Tail, View: DefaultThreadTranscriptView,
			HiddenCategories: threadMessagesHiddenCategories, HiddenSpecified: true,
			Before: before,
		})
		if err != nil {
			return nil, err
		}
		for index := len(response.Items) - 1; index >= 0 && len(newestFirst) < query.Tail && budget > 0; index-- {
			item := response.Items[index]
			role, ok := MessageRole(item.Type)
			if !ok || !query.Roles[role] {
				continue
			}
			retainedTruncated := item.OriginalBytes > history.MaxTranscriptBodyBytes
			message := Message{Role: role, At: item.CreatedAt, Text: item.Text, Complete: item.ContentRef == "",
				ItemID: item.ID, ContentRef: item.ContentRef, OriginalBytes: item.OriginalBytes,
				RetainedTruncated: retainedTruncated, ContentState: "inline", Preview: item.Text}
			if item.ContentRef != "" {
				message.Complete, message.ContentState = false, "inline_preview"
			}
			// Use the known original plus the exact JSON metadata as a lower bound:
			// an older body that cannot possibly fit is never loaded. A body that may
			// fit is checked against its actual encoded representation below, so
			// ordinary 50–100 KiB ASCII messages remain complete rather than being
			// rejected by a pessimistic escaping multiplier.
			if item.ContentRef != "" && item.OriginalBytes <= int64(history.MaxTranscriptBodyBytes) &&
				threadMessageBaseJSONBytes(message)+item.OriginalBytes <= budget {
				if bodies, ok := catalog.(transcriptBodyCatalog); ok {
					body, bodyErr := bodies.LoadTranscriptBody(ctx, id, item.ID)
					if bodyErr == nil {
						candidate := message
						candidate.Text, candidate.Complete = body.Text, !body.Truncated
						candidate.RetainedTruncated = body.Truncated
						if body.Truncated {
							candidate.ContentState = "retained_truncated_prefix"
						} else {
							candidate.ContentState = "retained_complete"
						}
						if threadMessageJSONBytes(candidate) <= budget {
							message = candidate
						}
					}
					// Retained-body lookup is optional enrichment. Keep the inline
					// preview for unavailable or transient body errors so the tail
					// endpoint remains useful even when a body row is gone.
				}
			}
			budget -= threadMessageJSONBytes(message)
			newestFirst = append(newestFirst, message)
		}
		if !response.Page.HasMoreBefore || response.Page.NextBeforeCursor == nil {
			break
		}
		before = *response.Page.NextBeforeCursor
	}
	messages := make([]Message, len(newestFirst))
	for index, message := range newestFirst {
		messages[len(newestFirst)-1-index] = message
	}
	return messages, nil
}

func threadMessageJSONBytes(message Message) int64 {
	encoded, _ := json.Marshal(message)
	return int64(len(encoded))
}

func threadMessageBaseJSONBytes(message Message) int64 {
	message.Text = ""
	return threadMessageJSONBytes(message)
}

// boundThreadMessages keeps the endpoint's response below the existing
// transcript budget. Bodies are downgraded to their explicit previews before
// any message is dropped, so callers never mistake a transport preview for the
// retained canonical text.
func boundThreadMessages(threadID string, messages []Message) []Message {
	for {
		encoded, _ := json.Marshal(messagesResponse{ThreadID: threadID, Count: len(messages), Messages: messages})
		if len(encoded)+1 <= MaxThreadTranscriptBytes || len(messages) == 0 {
			return messages
		}
		downgraded := false
		for index := 0; index < len(messages); index++ {
			if messages[index].Complete && messages[index].ContentRef != "" {
				messages[index].Text, messages[index].Complete, messages[index].ContentState = messages[index].Preview, false, "inline_preview"
				downgraded = true
				break
			}
		}
		if !downgraded {
			messages = messages[1:]
		}
	}
}

// MessageRole maps a projected item type onto the two roles this endpoint
// serves. Everything else — reasoning, tools, notices — is not a message.
func MessageRole(itemType string) (string, bool) {
	switch itemType {
	case "user_message":
		return MessageRoleUser, true
	case "agent_message":
		return MessageRoleAgent, true
	}
	return "", false
}

// renderThreadMessagesText is the token-cheap rendering: a role/time header per
// message and the text underneath, nothing else to parse past.
func renderThreadMessagesText(messages []Message) string {
	var out strings.Builder
	for _, message := range messages {
		out.WriteString(message.Role)
		if message.At != nil {
			out.WriteString(" [" + *message.At + "]")
		}
		if !message.Complete {
			out.WriteString(fmt.Sprintf(" [%s incomplete item=%s contentRef=%s originalBytes=%d retainedTruncated=%t]", message.ContentState, message.ItemID, message.ContentRef, message.OriginalBytes, message.RetainedTruncated))
		}
		out.WriteString("\n")
		out.WriteString(strings.TrimRight(message.Text, "\n"))
		out.WriteString("\n\n")
	}
	return out.String()
}

// WriteThreadReferenceError reports a failed id resolution: an ambiguous prefix
// names its candidates so the caller can retry with a longer one, and every
// other failure falls through to the shared catalog mapping.
func WriteThreadReferenceError(w http.ResponseWriter, err error) {
	var ambiguous *AmbiguousReferenceError
	if errors.As(err, &ambiguous) {
		httpapi.WriteErrorEnvelope(w, http.StatusBadRequest, httpapi.ErrorEnvelope{
			Error: httpapi.Error{Code: "ambiguous_thread_id", Message: fmt.Sprintf("Thread reference matches %d threads; use a longer prefix or an exact id",
				len(ambiguous.Candidates))},
			Candidates: ambiguous.Candidates,
		})

		return
	}
	WriteCatalogBackendError(w, err)
}
