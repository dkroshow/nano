package app

// thread_messages_api.go — the agent-ergonomic tail of a thread: GET
// /api/v1/threads/{id}/messages?tail=N&roles=user,agent&format=text returns
// clean chronological plain text, so "show me the last messages of thread X" is
// one curl with no cursors and no jq.
//
// It is a thin projection over the existing transcript service — no second
// parser, no direct SQLite read — and resolves ids the way an agent types them:
// an exact nano or native id, or an unambiguous prefix of either.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dkroshow/nano/internal/httpapi"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dkroshow/nano/internal/history"
)

const (
	defaultThreadMessagesTail = 20
	// maxThreadMessagesTail rides the transcript page limit: the tail is
	// assembled from transcript pages, so it cannot ask for more per page.
	maxThreadMessagesTail = maxThreadTranscriptLimit
	// maxThreadMessagePages bounds the walk backwards. A page can be mostly
	// non-message items, so one page does not always fill the tail; three is
	// generous without turning a tail request into a transcript scan.
	maxThreadMessagePages = 3
	// maxThreadReferenceCandidates bounds the list an ambiguous reference
	// reports back.
	maxThreadReferenceCandidates = 10

	threadMessagesFormatText = "text"
	threadMessagesFormatJSON = "json"

	threadMessageRoleUser  = "user"
	threadMessageRoleAgent = "agent"
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

// ambiguousThreadReferenceError is a prefix that matches more than one thread.
// It carries the candidates so the caller can pick, rather than guessing.
type ambiguousThreadReferenceError struct{ Candidates []string }

func (e *ambiguousThreadReferenceError) Error() string {
	return fmt.Sprintf("thread reference matches %d threads", len(e.Candidates))
}

// threadReferenceResolver is the optional id-resolution surface. A catalog
// without it (a stub in tests) simply takes the reference verbatim, which is
// exactly the exact-id case.
type threadReferenceResolver interface {
	ResolveThreadReference(ctx context.Context, reference string) (string, error)
}

type threadMessagesQuery struct {
	Tail   int
	Roles  map[string]bool
	Format string
}

type threadMessage struct {
	Role              string  `json:"role"`
	At                *string `json:"at"`
	Text              string  `json:"text"`
	Complete          bool    `json:"complete"`
	ItemID            string  `json:"itemId,omitempty"`
	ContentRef        string  `json:"contentRef,omitempty"`
	OriginalBytes     int64   `json:"originalBytes,omitempty"`
	RetainedTruncated bool    `json:"retainedTruncated"`
	ContentState      string  `json:"contentState"`
	preview           string
}

type threadMessagesResponse struct {
	ThreadID string          `json:"threadId"`
	Count    int             `json:"count"`
	Messages []threadMessage `json:"messages"`
}

func (s *server) handleThreadMessages(w http.ResponseWriter, r *http.Request, rawID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(threadTranscriptCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	reference, err := url.PathUnescape(rawID)
	if err != nil || strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	query, err := parseThreadMessagesQuery(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	// nht_ references: hop from the hosted
	// record to its current native id, then resolve that through the catalog
	// like any other native id. A hosted record without a native id yet has no
	// transcript to page.
	if hostedThreadReferencePattern.MatchString(reference) {
		nativeID, ok := s.hostedThreadNativeID(reference)
		if !ok {
			httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Unknown hosted thread")
			return
		}
		if nativeID == "" {
			httpapi.WriteError(w, http.StatusNotFound, "thread_not_indexed", "Hosted thread has no native transcript yet")
			return
		}
		reference = nativeID
	}
	id, err := s.resolveThreadReference(r.Context(), reference)
	if err != nil {
		writeThreadReferenceError(w, err)
		return
	}
	messages, err := collectThreadMessages(r.Context(), catalog, id, query)
	if err != nil {
		writeTranscriptBackendError(w, err)
		return
	}
	messages = boundThreadMessages(id, messages)
	if query.Format == threadMessagesFormatJSON {
		writeCatalogJSON(w, http.StatusOK, threadMessagesResponse{ThreadID: id, Count: len(messages), Messages: messages})
		return
	}
	for len(messages) > 0 && len(renderThreadMessagesText(messages)) > maxThreadTranscriptBytes {
		messages = messages[1:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, renderThreadMessagesText(messages))
}

// resolveThreadReference resolves the path segment to a durable thread id. A
// catalog without the resolver takes the reference as an exact id — the only
// interpretation a stub can honour.
func (s *server) resolveThreadReference(ctx context.Context, reference string) (string, error) {
	resolver, ok := s.catalog.(threadReferenceResolver)
	if !ok {
		return reference, nil
	}
	return resolver.ResolveThreadReference(ctx, reference)
}

func parseThreadMessagesQuery(r *http.Request) (threadMessagesQuery, error) {
	q := r.URL.Query()
	query := threadMessagesQuery{
		Tail:   defaultThreadMessagesTail,
		Roles:  map[string]bool{threadMessageRoleUser: true, threadMessageRoleAgent: true},
		Format: threadMessagesFormatText,
	}
	if raw := strings.TrimSpace(q.Get("tail")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return query, errors.New("tail must be a positive integer")
		}
		query.Tail = value
	}
	if query.Tail > maxThreadMessagesTail {
		query.Tail = maxThreadMessagesTail
	}
	if raw := strings.TrimSpace(q.Get("roles")); raw != "" {
		roles := map[string]bool{}
		for _, value := range strings.Split(raw, ",") {
			switch value = strings.TrimSpace(value); value {
			case threadMessageRoleUser, threadMessageRoleAgent:
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
		if raw != threadMessagesFormatText && raw != threadMessagesFormatJSON {
			return query, errors.New("format must be text or json")
		}
		query.Format = raw
	}
	return query, nil
}

// collectThreadMessages walks transcript pages backwards until it has the tail
// the caller asked for, then returns it in chronological order. Walking is
// bounded twice — by page count and by the transcript response budget — so an
// enormous thread costs the same as a small one.
func collectThreadMessages(ctx context.Context, catalog threadTranscriptCatalog, id string, query threadMessagesQuery) ([]threadMessage, error) {
	newestFirst := make([]threadMessage, 0, query.Tail)
	budget := int64(maxThreadTranscriptBytes - threadMessagesResponseReserve)
	before := ""
	for page := 0; page < maxThreadMessagePages && len(newestFirst) < query.Tail && budget > 0; page++ {
		response, err := catalog.LoadTranscript(ctx, id, transcriptListQuery{
			Limit: query.Tail, View: defaultThreadTranscriptView,
			HiddenCategories: threadMessagesHiddenCategories, HiddenSpecified: true,
			Before: before,
		})
		if err != nil {
			return nil, err
		}
		for index := len(response.Items) - 1; index >= 0 && len(newestFirst) < query.Tail && budget > 0; index-- {
			item := response.Items[index]
			role, ok := threadMessageRole(item.Type)
			if !ok || !query.Roles[role] {
				continue
			}
			retainedTruncated := item.OriginalBytes > history.MaxTranscriptBodyBytes
			message := threadMessage{Role: role, At: item.CreatedAt, Text: item.Text, Complete: item.ContentRef == "",
				ItemID: item.ID, ContentRef: item.ContentRef, OriginalBytes: item.OriginalBytes,
				RetainedTruncated: retainedTruncated, ContentState: "inline", preview: item.Text}
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
				if bodies, ok := catalog.(threadTranscriptBodyCatalog); ok {
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
	messages := make([]threadMessage, len(newestFirst))
	for index, message := range newestFirst {
		messages[len(newestFirst)-1-index] = message
	}
	return messages, nil
}

func threadMessageJSONBytes(message threadMessage) int64 {
	encoded, _ := json.Marshal(message)
	return int64(len(encoded))
}

func threadMessageBaseJSONBytes(message threadMessage) int64 {
	message.Text = ""
	return threadMessageJSONBytes(message)
}

// boundThreadMessages keeps the endpoint's response below the existing
// transcript budget. Bodies are downgraded to their explicit previews before
// any message is dropped, so callers never mistake a transport preview for the
// retained canonical text.
func boundThreadMessages(threadID string, messages []threadMessage) []threadMessage {
	for {
		encoded, _ := json.Marshal(threadMessagesResponse{ThreadID: threadID, Count: len(messages), Messages: messages})
		if len(encoded)+1 <= maxThreadTranscriptBytes || len(messages) == 0 {
			return messages
		}
		downgraded := false
		for index := 0; index < len(messages); index++ {
			if messages[index].Complete && messages[index].ContentRef != "" {
				messages[index].Text, messages[index].Complete, messages[index].ContentState = messages[index].preview, false, "inline_preview"
				downgraded = true
				break
			}
		}
		if !downgraded {
			messages = messages[1:]
		}
	}
}

// threadMessageRole maps a projected item type onto the two roles this endpoint
// serves. Everything else — reasoning, tools, notices — is not a message.
func threadMessageRole(itemType string) (string, bool) {
	switch itemType {
	case "user_message":
		return threadMessageRoleUser, true
	case "agent_message":
		return threadMessageRoleAgent, true
	}
	return "", false
}

// renderThreadMessagesText is the token-cheap rendering: a role/time header per
// message and the text underneath, nothing else to parse past.
func renderThreadMessagesText(messages []threadMessage) string {
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

// writeThreadReferenceError reports a failed id resolution: an ambiguous prefix
// names its candidates so the caller can retry with a longer one, and every
// other failure falls through to the shared catalog mapping.
func writeThreadReferenceError(w http.ResponseWriter, err error) {
	var ambiguous *ambiguousThreadReferenceError
	if errors.As(err, &ambiguous) {
		httpapi.WriteErrorEnvelope(w, http.StatusBadRequest, httpapi.ErrorEnvelope{
			Error: httpapi.Error{Code: "ambiguous_thread_id", Message: fmt.Sprintf("Thread reference matches %d threads; use a longer prefix or an exact id",
				len(ambiguous.Candidates))},
			Candidates: ambiguous.Candidates,
		})

		return
	}
	writeCatalogBackendError(w, err)
}

// hostedThreadNativeID returns the current native thread id for a hosted id,
// preferring the live session, then the durable execution record. The second
// return is false when no hosted record exists at all.
func (s *server) hostedThreadNativeID(hostedID string) (string, bool) {
	if live, ok := s.liveHostedSession(hostedID); ok {
		return live.ThreadID, true
	}
	if s.agentExecutions == nil {
		return "", false
	}
	record, ok := s.agentExecutions.session(hostedID)
	if !ok {
		return "", false
	}
	return record.NativeThreadID, true
}
