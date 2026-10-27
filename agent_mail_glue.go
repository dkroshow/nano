package main

import (
	"context"

	featuremail "github.com/dkroshow/nano/internal/features/mail"
)

const (
	agentMessageUnread   = featuremail.Unread
	agentMessageSeen     = featuremail.Seen
	agentMessageActed    = featuremail.Acted
	agentMessageDeclined = featuremail.Declined
	agentMessageDeferred = featuremail.Deferred

	maxAgentMessageSubjectBytes = featuremail.MaxSubjectBytes
	maxAgentMessageBodyBytes    = featuremail.MaxBodyBytes
	maxAgentMessageNoteBytes    = featuremail.MaxNoteBytes
	maxAgentMessageRefs         = featuremail.MaxRefs
)

var (
	errAgentMessageNotFound    = featuremail.ErrNotFound
	errUnreadFooterUnavailable = featuremail.ErrUnreadNoticeUnavailable
)

type agentMessageStore = featuremail.Store
type agentMailMigrationReport = featuremail.MigrationReport
type agentMessageRef = featuremail.Ref
type agentMessageSenderView = featuremail.SenderView
type agentMessageRecipientView = featuremail.RecipientView
type agentMessageView = featuremail.Message
type agentMessageEventView = featuremail.EventView
type agentMessageDetailView = featuremail.DetailView
type agentMessageCreate = featuremail.Create
type agentMessageListOptions = featuremail.ListOptions
type agentMessageStoreError = featuremail.StoreError
type agentMailCounts = featuremail.Counts

func openAgentMessageStore(path, legacyPath string) (*agentMessageStore, agentMailMigrationReport, error) {
	return featuremail.OpenStore(path, legacyPath)
}

func startAgentMailRetention(ctx context.Context, store *agentMessageStore, logf func(string, ...any)) {
	featuremail.StartRetention(ctx, store, logf)
}

func normalizedAgentMessageRefs(refs []agentMessageRef) []agentMessageRef {
	return featuremail.NormalizeRefs(refs)
}

func agentMessageRequestHashWithFeedback(recipient, subject, body string, refs []agentMessageRef, dispositionFeedback bool, feedbackFor string) string {
	return featuremail.RequestHashWithFeedback(recipient, subject, body, refs, dispositionFeedback, feedbackFor)
}

func (s *server) promptWithUnreadFooter(hostedThreadID, prompt string) (string, error) {
	if hostedThreadID == "" {
		return prompt, nil
	}
	return s.notices.Append(hostedThreadID, prompt), nil
}

func (sess *session) promptWithUnreadFooter(prompt string) (string, error) {
	if sess == nil || sess.hostedThreadID == "" {
		return prompt, nil
	}
	return sess.notices.Append(sess.hostedThreadID, prompt), nil
}
