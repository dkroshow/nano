package interact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	DefaultOrigin = "http://127.0.0.1:7690"
	maxErrorBytes = 8 << 10
)

type HTTPClient struct {
	origin string
	client *http.Client
}

func NewHTTPClient(origin string, client *http.Client) *HTTPClient {
	if strings.TrimSpace(origin) == "" {
		origin = DefaultOrigin
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPClient{origin: strings.TrimRight(origin, "/"), client: client}
}

var (
	_ PromptSubmitter         = (*HTTPClient)(nil)
	_ MessageSender           = (*HTTPClient)(nil)
	_ CompletedTurnSubscriber = (*HTTPClient)(nil)
	_ TranscriptReader        = (*HTTPClient)(nil)
	_ LifecycleReader         = (*HTTPClient)(nil)
)

func (c *HTTPClient) SendMessage(ctx context.Context, request SendMessageRequest) (json.RawMessage, error) {
	var message json.RawMessage
	err := c.doJSON(ctx, http.MethodPost, hostedPath(request.RecipientThreadRef, "/messages"), nil, request, &message)
	return message, err
}

func (c *HTTPClient) ReadInbox(ctx context.Context, request MessagePageRequest) (json.RawMessage, error) {
	query := messagePageQuery(request)
	query.Set("actorThreadRef", request.ActorRef)
	if request.Peek {
		query.Set("peek", "true")
	}
	var page json.RawMessage
	err := c.doJSON(ctx, http.MethodGet, hostedPath(request.ThreadRef, "/inbox"), query, nil, &page)
	return page, err
}

func (c *HTTPClient) ReadOutbox(ctx context.Context, request MessagePageRequest) (json.RawMessage, error) {
	var page json.RawMessage
	err := c.doJSON(ctx, http.MethodGet, hostedPath(request.ThreadRef, "/outbox"), messagePageQuery(request), nil, &page)
	return page, err
}

func (c *HTTPClient) ReadMessage(ctx context.Context, messageID string) (json.RawMessage, error) {
	var detail json.RawMessage
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/messages/"+url.PathEscape(messageID), nil, nil, &detail)
	return detail, err
}

func messagePageQuery(request MessagePageRequest) url.Values {
	query := url.Values{}
	if request.Since > 0 {
		query.Set("since", strconv.FormatInt(request.Since, 10))
	}
	if request.Limit > 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	if request.Status != "" {
		query.Set("status", request.Status)
	}
	if request.Ref != "" {
		query.Set("ref", request.Ref)
	}
	return query
}

func (c *HTTPClient) SubmitPrompt(ctx context.Context, request SubmitPromptRequest) (Turn, error) {
	var turn Turn
	err := c.doJSON(ctx, http.MethodPost, hostedPath(request.ThreadRef, "/turns"), nil, request, &turn)
	return turn, err
}

func (c *HTTPClient) CreateHostedThread(ctx context.Context, request HostedThreadCreateRequest) (HostedThread, error) {
	var thread HostedThread
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/hosted-threads", nil, request, &thread)
	return thread, err
}

func (c *HTTPClient) CloseHostedThread(ctx context.Context, threadRef string) error {
	var thread HostedThread
	return c.doJSON(ctx, http.MethodPost, hostedPath(threadRef, "/close"), nil, struct{}{}, &thread)
}

func (c *HTTPClient) ReadTurn(ctx context.Context, operationID string, afterRevision int64, waitMS int) (Turn, error) {
	query := url.Values{}
	if afterRevision > 0 {
		query.Set("afterRevision", strconv.FormatInt(afterRevision, 10))
	}
	if waitMS > 0 {
		query.Set("waitMs", strconv.Itoa(waitMS))
	}
	var turn Turn
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/turn-operations/"+url.PathEscape(operationID), query, nil, &turn)
	return turn, err
}

func (c *HTTPClient) ResolveIdentity(ctx context.Context, ref string) (Identity, error) {
	query := url.Values{"ref": []string{ref}}
	var identity Identity
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/identity/resolve", query, nil, &identity)
	return identity, err
}

func (c *HTTPClient) SubscribeCompletedTurns(ctx context.Context, request PageRequest) (CompletedTurnPage, error) {
	query := url.Values{}
	if request.Cursor != "" {
		query.Set("after", request.Cursor)
	}
	if request.Limit > 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	var page CompletedTurnPage
	err := c.doJSON(ctx, http.MethodGet, hostedPath(request.ThreadRef, "/completed-turns"), query, nil, &page)
	return page, err
}

func (c *HTTPClient) ReadTranscript(ctx context.Context, request PageRequest) (TranscriptWindow, error) {
	query := url.Values{}
	if request.Cursor != "" {
		query.Set("before", request.Cursor)
	}
	if request.Limit > 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	var window TranscriptWindow
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/threads/"+url.PathEscape(request.ThreadRef)+"/transcript", query, nil, &window)
	return window, err
}

func (c *HTTPClient) ReadLifecycle(ctx context.Context, threadRef string) (LifecycleState, error) {
	var state LifecycleState
	err := c.doJSON(ctx, http.MethodGet, hostedPath(threadRef, ""), nil, nil, &state)
	return state, err
}

// ReadAvailability uses the dedicated hard-facts route while returning the
// same four lifecycle facts that feature clients consume.
func (c *HTTPClient) ReadAvailability(ctx context.Context, threadRef string) (LifecycleState, error) {
	var response struct {
		State   string   `json:"state"`
		Reasons []string `json:"reasons"`
		Facts   struct {
			LastHumanMessageAt string `json:"lastHumanMessageAt"`
			Attachment         string `json:"attachment"`
		} `json:"facts"`
	}
	err := c.doJSON(ctx, http.MethodGet, hostedPath(threadRef, "/availability"), nil, nil, &response)
	return LifecycleState{
		Availability:        response.State,
		AvailabilityReasons: response.Reasons,
		LastHumanMessageAt:  response.Facts.LastHumanMessageAt,
		AttachmentState:     response.Facts.Attachment,
	}, err
}

func hostedPath(threadRef, suffix string) string {
	return "/api/v1/hosted-threads/" + url.PathEscape(threadRef) + suffix
}

func (c *HTTPClient) doJSON(ctx context.Context, method, path string, query url.Values, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := c.origin + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes))
		return fmt.Errorf("%s %s: %s: %s", method, path, response.Status, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}
