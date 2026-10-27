// nano-agent is a thin localhost client for Nano's provider-neutral hosted
// thread API. It deliberately owns no queue, retry policy, or worker profile;
// callers keep those decisions and use this command as transport.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"
)

type client struct {
	base string
	http *http.Client
}

type turn struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Terminal  bool   `json:"terminal"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updatedAt"`
}

type hostedThread struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updatedAt"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	global := flag.NewFlagSet("nano-agent", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	base := global.String("base", "http://127.0.0.1:7690", "Nano base URL")
	if err := global.Parse(os.Args[1:]); err != nil {
		usage()
		exitErr(err)
	}
	if len(global.Args()) == 0 {
		usage()
		exitErr(errors.New("command required"))
	}
	c := &client{base: strings.TrimRight(*base, "/"), http: &http.Client{Timeout: 35 * time.Second}}
	args := global.Args()
	var err error
	switch args[0] {
	case "capabilities":
		err = c.printRequest(ctx, http.MethodGet, "/api/v1/capabilities", nil)
	case "list":
		err = c.printRequest(ctx, http.MethodGet, "/api/v1/hosted-threads", nil)
	case "start":
		err = runStart(ctx, c, args[1:])
	case "run":
		err = runTurn(ctx, c, args[1:])
	case "send":
		err = runMessageSend(ctx, c, args[1:])
	case "inbox":
		err = runMessageInbox(ctx, c, args[1:])
	case "outbox":
		err = runMessageOutbox(ctx, c, args[1:])
	case "message":
		err = runMessageShow(ctx, c, args[1:])
	case "ack":
		err = runMessageAck(ctx, c, args[1:])
	case "assign":
		err = runAssignment(ctx, c, args[1:])
	case "resolve":
		err = runResolve(ctx, c, args[1:])
	case "status":
		err = runStatus(ctx, c, args[1:])
	case "heartbeat":
		err = runHeartbeat(ctx, c, args[1:])
	case "answer":
		err = runAnswer(ctx, c, args[1:])
	case "interrupt":
		err = runSessionAction(ctx, c, args[1:], "interrupt", http.MethodPost)
	case "close":
		// close means "end the attachment, keep the durable record" — the verb
		// split moved that meaning to POST {ref}/close; DELETE now destroys.
		err = runSessionAction(ctx, c, args[1:], "close", http.MethodPost)
	case "detach":
		err = runSessionAction(ctx, c, args[1:], "detach", http.MethodPost)
	case "delete":
		err = runSessionAction(ctx, c, args[1:], "", http.MethodDelete)
	default:
		usage()
		err = fmt.Errorf("unknown command %q", args[0])
	}
	exitErr(err)
}

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }
func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func expandMessageSelf(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref != "me" {
		return ref, nil
	}
	value := strings.TrimSpace(os.Getenv("NANO_HOSTED_THREAD_ID"))
	if value == "" {
		return "", errors.New("me requires NANO_HOSTED_THREAD_ID")
	}
	return value, nil
}

func runMessageSend(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	to := fs.String("to", "", "recipient hosted thread reference")
	from := fs.String("from", "", "sender hosted thread reference or me")
	clientID := fs.String("client-id", "", "stable sender-selected message id")
	subject := fs.String("subject", "", "optional subject")
	body := fs.String("body", "", "message body; reads stdin when empty")
	var rawRefs stringListFlag
	fs.Var(&rawRefs, "ref", "opaque kind:id reference; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*to) == "" || strings.TrimSpace(*from) == "" || strings.TrimSpace(*clientID) == "" {
		return errors.New("send requires --to, --from, and --client-id")
	}
	var err error
	*from, err = expandMessageSelf(*from)
	if err != nil {
		return err
	}
	if *body == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		*body = string(data)
	}
	if strings.TrimSpace(*body) == "" {
		return errors.New("send requires --body or body text on stdin")
	}
	refs := make([]map[string]string, 0, len(rawRefs))
	for _, raw := range rawRefs {
		kind, id, ok := strings.Cut(raw, ":")
		if !ok || strings.TrimSpace(kind) == "" || strings.TrimSpace(id) == "" {
			return fmt.Errorf("invalid --ref %q; want kind:id", raw)
		}
		refs = append(refs, map[string]string{"kind": strings.TrimSpace(kind), "id": strings.TrimSpace(id)})
	}
	bodyValue := map[string]any{"clientMessageId": *clientID, "senderThreadRef": *from, "body": *body}
	putIf(bodyValue, "subject", *subject)
	if len(refs) > 0 {
		bodyValue["refs"] = refs
	}
	return c.printRequest(ctx, http.MethodPost, "/api/v1/hosted-threads/"+url.PathEscape(*to)+"/messages", bodyValue)
}

func messageListQuery(status, ref string, since int64, limit int) string {
	values := url.Values{}
	if status != "" {
		values.Set("status", status)
	}
	if ref != "" {
		values.Set("ref", ref)
	}
	if since > 0 {
		values.Set("since", fmt.Sprint(since))
	}
	values.Set("limit", fmt.Sprint(limit))
	return values.Encode()
}

func runMessageInbox(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	as := fs.String("as", "", "inbox owner reference or me")
	status := fs.String("status", "", "comma-separated statuses")
	ref := fs.String("ref", "", "kind:id reference filter")
	since := fs.Int64("since", 0, "return recipient sequences after this value")
	limit := fs.Int("limit", 50, "maximum messages")
	peek := fs.Bool("peek", false, "read without marking unread mail seen")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*as) == "" {
		return errors.New("inbox requires --as")
	}
	var err error
	*as, err = expandMessageSelf(*as)
	if err != nil {
		return err
	}
	values, _ := url.ParseQuery(messageListQuery(*status, *ref, *since, *limit))
	values.Set("actorThreadRef", *as)
	if *peek {
		values.Set("peek", "true")
	}
	path := "/api/v1/hosted-threads/" + url.PathEscape(*as) + "/inbox?" + values.Encode()
	return c.printRequest(ctx, http.MethodGet, path, nil)
}

func runMessageOutbox(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("outbox", flag.ContinueOnError)
	as := fs.String("as", "", "sender reference or me")
	status := fs.String("status", "", "comma-separated statuses")
	ref := fs.String("ref", "", "kind:id reference filter")
	since := fs.Int64("since", 0, "return global message rows after this value")
	limit := fs.Int("limit", 50, "maximum messages")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*as) == "" {
		return errors.New("outbox requires --as")
	}
	var err error
	*as, err = expandMessageSelf(*as)
	if err != nil {
		return err
	}
	path := "/api/v1/hosted-threads/" + url.PathEscape(*as) + "/outbox?" + messageListQuery(*status, *ref, *since, *limit)
	return c.printRequest(ctx, http.MethodGet, path, nil)
}

func runMessageShow(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("message", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("message requires one nmsg_ id")
	}
	return c.printRequest(ctx, http.MethodGet, "/api/v1/messages/"+url.PathEscape(fs.Arg(0)), nil)
}

func runMessageAck(ctx context.Context, c *client, args []string) error {
	messageID := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		messageID, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	as := fs.String("as", "", "recipient reference or me")
	status := fs.String("status", "", "acted, declined, or deferred")
	note := fs.String("note", "", "optional disposition note")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if messageID == "" && fs.NArg() == 1 {
		messageID = fs.Arg(0)
	}
	if messageID == "" || fs.NArg() > 1 || strings.TrimSpace(*as) == "" || strings.TrimSpace(*status) == "" {
		return errors.New("ack requires one nmsg_ id plus --as and --status")
	}
	var err error
	*as, err = expandMessageSelf(*as)
	if err != nil {
		return err
	}
	body := map[string]any{"actorThreadRef": *as, "status": *status}
	putIf(body, "note", *note)
	return c.printRequest(ctx, http.MethodPost, "/api/v1/messages/"+url.PathEscape(messageID)+"/status", body)
}

func runHeartbeat(ctx context.Context, c *client, args []string) error {
	if len(args) == 0 {
		return errors.New("heartbeat requires list, attach, templates, scripts, pause, resume, remove, or log")
	}
	switch args[0] {
	case "list":
		return c.printRequest(ctx, http.MethodGet, "/api/v1/heartbeat/attachments", nil)
	case "templates":
		return c.printRequest(ctx, http.MethodGet, "/api/v1/heartbeat/script-templates", nil)
	case "scripts":
		return c.printRequest(ctx, http.MethodGet, "/api/v1/heartbeat/scripts", nil)
	case "attach":
		fs := flag.NewFlagSet("heartbeat attach", flag.ContinueOnError)
		target := fs.String("target", "", "watched hosted thread reference")
		cadence := fs.Int("cadence", 0, "act every N wall-clock minutes")
		offset := fs.Int("offset", 0, "minute offset within the cadence, 0 <= offset < cadence")
		prompt := fs.String("prompt", "", "nudge prompt; reads stdin when empty")
		templateID := fs.String("template", "", "optional repository pre-ping template id")
		settings := fs.String("settings", "{}", "attachment settings JSON object for the pre-ping script")
		authority := fs.String("authority", "", "recorded authority evidence; Nano does not enforce it")
		actorName := fs.String("actor", "local-operator", "local operator name recorded in the audit log")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *target == "" || *cadence < 1 {
			return errors.New("heartbeat attach requires --target and positive --cadence")
		}
		if *prompt == "" && *templateID == "" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			*prompt = string(data)
		}
		if strings.TrimSpace(*prompt) == "" && *templateID == "" {
			return errors.New("heartbeat attach requires --prompt or stdin")
		}
		body := map[string]any{"targetHostedThreadId": *target, "cadenceMinutes": *cadence, "offsetMinutes": *offset, "prompt": *prompt, "authority": *authority, "actor": map[string]string{"kind": "local-operator", "name": *actorName}}
		if *templateID != "" {
			var settingsValue map[string]any
			if err := json.Unmarshal([]byte(*settings), &settingsValue); err != nil || settingsValue == nil {
				return errors.New("--settings must be a JSON object")
			}
			body["prePing"] = map[string]any{"templateId": *templateID, "settings": settingsValue}
		}
		return c.printRequest(ctx, http.MethodPost, "/api/v1/heartbeat/attachments", body)
	case "pause", "resume", "remove":
		action := args[0]
		fs := flag.NewFlagSet("heartbeat "+action, flag.ContinueOnError)
		id := fs.String("id", "", "heartbeat attachment id")
		reason := fs.String("reason", "", "audit reason")
		actorName := fs.String("actor", "local-operator", "local operator name recorded in the audit log")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" || strings.TrimSpace(*reason) == "" {
			return fmt.Errorf("heartbeat %s requires --id and --reason", action)
		}
		method, suffix := http.MethodPost, "/"+action
		if action == "remove" {
			method, suffix = http.MethodDelete, ""
		}
		body := map[string]any{"reason": *reason, "actor": map[string]string{"kind": "local-operator", "name": *actorName}}
		return c.printRequest(ctx, method, "/api/v1/heartbeat/attachments/"+url.PathEscape(*id)+suffix, body)
	case "log":
		fs := flag.NewFlagSet("heartbeat log", flag.ContinueOnError)
		id := fs.String("id", "", "heartbeat attachment id; omit for every attachment")
		limit := fs.Int("limit", 100, "maximum decisions to return")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		query := "?limit=" + url.QueryEscape(fmt.Sprint(*limit))
		if *id != "" {
			query += "&attachmentId=" + url.QueryEscape(*id)
		}
		return c.printRequest(ctx, http.MethodGet, "/api/v1/heartbeat/log"+query, nil)
	default:
		return fmt.Errorf("unknown heartbeat command %q", args[0])
	}
}

func runStart(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	sessionID := fs.String("session", "", "stable client creation alias (required)")
	provider := fs.String("provider", "", "codex, claude, or experimental grok (required)")
	invoker := fs.String("invoker", "", "invoking agent's hosted, native, or current Nano thread id (required)")
	background := fs.Bool("background", false, "fold the session out of Nano's compact sidebar")
	cwd := fs.String("cwd", "", "working directory")
	model := fs.String("model", "", "provider model")
	effort := fs.String("effort", "", "reasoning effort")
	sandbox := fs.String("sandbox", "", "Codex sandbox")
	approval := fs.String("approval", "", "Codex approval policy")
	permissionMode := fs.String("permission-mode", "", "Claude or Grok permission mode")
	allowedTools := fs.String("allowed-tools", "", "Claude allowed tools")
	assignmentRole := fs.String("assignment-role", "", "optional worker or auditor assignment role")
	focus := fs.String("focus", "", "assignment focus; required with --assignment-role")
	relatedThread := fs.String("related-thread", "", "hosted thread to place this assignment under")
	auditTargets := fs.String("audit-targets", "", "comma-separated hosted thread ids for an auditor assignment")
	wait := fs.Duration("wait", time.Minute, "wait for ready; zero disables waiting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionID == "" || *provider == "" || *invoker == "" {
		return errors.New("start requires --session, --provider, and --invoker")
	}
	mode := "shared"
	if *background {
		mode = "background"
	}
	body := map[string]any{
		"clientSessionId": *sessionID, "provider": *provider, "mode": "start",
		"presentationMode": mode, "invokerThreadRef": *invoker,
	}
	putIf(body, "cwd", *cwd)
	putIf(body, "model", *model)
	putIf(body, "effort", *effort)
	putIf(body, "sandbox", *sandbox)
	putIf(body, "approval", *approval)
	putIf(body, "permissionMode", *permissionMode)
	putIf(body, "allowedTools", *allowedTools)
	if *assignmentRole != "" || *focus != "" || *relatedThread != "" || *auditTargets != "" {
		assignment, err := cliAssignment(*assignmentRole, *focus, *relatedThread, *auditTargets)
		if err != nil {
			return err
		}
		body["assignment"] = assignment
	}
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodPost, "/api/v1/hosted-threads", body, &raw); err != nil {
		return err
	}
	var session hostedThread
	if err := json.Unmarshal(raw, &session); err != nil {
		return fmt.Errorf("decode hosted thread: %w", err)
	}
	if *wait <= 0 || session.Status == "ready" {
		return printJSON(raw)
	}
	deadline := time.NewTimer(*wait)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		switch session.Status {
		case "exited", "creation_unknown":
			_ = printJSON(raw)
			return fmt.Errorf("hosted thread %s: %s", session.Status, session.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			_ = printJSON(raw)
			return fmt.Errorf("hosted thread was not ready after %s", *wait)
		case <-ticker.C:
			path := "/api/v1/hosted-threads/" + url.PathEscape(*sessionID)
			if err := c.request(ctx, http.MethodGet, path, nil, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &session); err != nil {
				return fmt.Errorf("decode hosted thread: %w", err)
			}
			if session.Status == "ready" {
				return printJSON(raw)
			}
		}
	}
}

func runTurn(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sessionID := fs.String("session", "", "hosted thread id, client alias, Nano session id, or live native id (required)")
	actionID := fs.String("action", "", "stable caller action id (required)")
	sender := fs.String("sender", "", "your hosted, native, or current Nano thread id (required)")
	prompt := fs.String("prompt", "", "prompt text; reads stdin when empty")
	wait := fs.Bool("wait", false, "keep this process in the foreground until the provider turn stops")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionID == "" || *actionID == "" || strings.TrimSpace(*sender) == "" {
		return errors.New("run requires --session, --action, and --sender")
	}
	if *prompt == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		*prompt = string(data)
	}
	if strings.TrimSpace(*prompt) == "" {
		return errors.New("run requires --prompt or prompt text on stdin")
	}
	var initial json.RawMessage
	path := "/api/v1/hosted-threads/" + url.PathEscape(*sessionID) + "/turns"
	if err := c.request(ctx, http.MethodPost, path, map[string]any{
		"clientActionId": *actionID, "prompt": *prompt, "senderThreadRef": *sender,
	}, &initial); err != nil {
		return err
	}
	var op turn
	if err := json.Unmarshal(initial, &op); err != nil {
		return fmt.Errorf("decode turn operation: %w", err)
	}
	if !*wait || turnWaitFinished(op) {
		return printJSON(initial)
	}
	for !turnWaitFinished(op) {
		path = fmt.Sprintf("/api/v1/turn-operations/%s?afterRevision=%d&waitMs=30000", url.PathEscape(op.ID), op.Revision)
		if err := c.request(ctx, http.MethodGet, path, nil, &op); err != nil {
			return err
		}
	}
	var complete json.RawMessage
	if err := c.request(ctx, http.MethodGet, "/api/v1/turn-operations/"+url.PathEscape(op.ID), nil, &complete); err != nil {
		return err
	}
	return printJSON(complete)
}

func runAssignment(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("assign", flag.ContinueOnError)
	sessionID := fs.String("session", "", "hosted thread id or resolvable reference (required)")
	actionID := fs.String("action", "", "stable assignment update id (required)")
	role := fs.String("assignment-role", "", "worker or auditor")
	focus := fs.String("focus", "", "assignment focus")
	relatedThread := fs.String("related-thread", "", "hosted thread to place this assignment under")
	targets := fs.String("audit-targets", "", "comma-separated hosted thread ids")
	clear := fs.Bool("clear", false, "clear the current assignment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionID == "" || *actionID == "" {
		return errors.New("assign requires --session and --action")
	}
	body := map[string]any{"clientActionId": *actionID}
	if *clear {
		if *role != "" || *focus != "" || *relatedThread != "" || *targets != "" {
			return errors.New("--clear cannot be combined with assignment fields")
		}
		body["assignment"] = nil
	} else {
		assignment, err := cliAssignment(*role, *focus, *relatedThread, *targets)
		if err != nil {
			return err
		}
		body["assignment"] = assignment
	}
	return c.printRequest(ctx, http.MethodPut, "/api/v1/hosted-threads/"+url.PathEscape(*sessionID)+"/assignment", body)
}

func cliAssignment(role, focus, relatedThread, targets string) (map[string]any, error) {
	if role != "worker" && role != "auditor" {
		return nil, errors.New("--assignment-role must be worker or auditor")
	}
	if strings.TrimSpace(focus) == "" {
		return nil, errors.New("--focus is required with an assignment")
	}
	items := []map[string]string{}
	for _, target := range strings.Split(targets, ",") {
		if target = strings.TrimSpace(target); target != "" {
			items = append(items, map[string]string{"hostedThreadId": target})
		}
	}
	if role == "worker" && len(items) != 0 {
		return nil, errors.New("worker assignments cannot have --audit-targets")
	}
	assignment := map[string]any{"assignmentRole": role, "focus": focus, "auditTargets": items}
	if relatedThread = strings.TrimSpace(relatedThread); relatedThread != "" {
		assignment["relatedHostedThreadId"] = relatedThread
	}
	return assignment, nil
}

func turnWaitFinished(op turn) bool {
	return op.Terminal || op.Status == "delivery_unknown"
}

func runResolve(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("resolve requires a display name, native thread id, or hosted thread id")
	}
	path := "/api/v1/identity/resolve?" + url.Values{"ref": {fs.Arg(0)}}.Encode()
	return c.printRequest(ctx, http.MethodGet, path, nil)
}

func runStatus(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	operationID := fs.String("operation", "", "turn operation id")
	sessionID := fs.String("session", "", "hosted thread id or resolvable live reference")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *operationID != "":
		return c.printRequest(ctx, http.MethodGet, "/api/v1/turn-operations/"+url.PathEscape(*operationID), nil)
	case *sessionID != "":
		return c.printRequest(ctx, http.MethodGet, "/api/v1/hosted-threads/"+url.PathEscape(*sessionID), nil)
	default:
		return errors.New("status requires --operation or --session")
	}
}

func runAnswer(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	sessionID := fs.String("session", "", "hosted thread id or resolvable live reference (required)")
	requestID := fs.String("request", "", "pending interaction id (required)")
	decision := fs.String("decision", "", "accept, acceptForSession, decline, or cancel (required)")
	answers := fs.String("answers", "", "JSON object of answers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionID == "" || *requestID == "" || *decision == "" {
		return errors.New("answer requires --session, --request, and --decision")
	}
	body := map[string]any{"decision": *decision}
	if *answers != "" {
		var decoded any
		if err := json.Unmarshal([]byte(*answers), &decoded); err != nil {
			return fmt.Errorf("parse --answers: %w", err)
		}
		body["answers"] = decoded
	}
	path := "/api/v1/hosted-threads/" + url.PathEscape(*sessionID) + "/interactions/" + url.PathEscape(*requestID)
	return c.printRequest(ctx, http.MethodPost, path, body)
}

func runSessionAction(ctx context.Context, c *client, args []string, action, method string) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	sessionID := fs.String("session", "", "hosted thread id or resolvable live reference (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionID == "" {
		return fmt.Errorf("%s requires --session", firstNonEmpty(action, "close"))
	}
	path := "/api/v1/hosted-threads/" + url.PathEscape(*sessionID)
	if action != "" {
		path += "/" + action
	}
	return c.printRequest(ctx, method, path, nil)
}

func (c *client) printRequest(ctx context.Context, method, path string, body any) error {
	var result json.RawMessage
	if err := c.request(ctx, method, path, body, &result); err != nil {
		return err
	}
	return printJSON(result)
}

func (c *client) request(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Nano %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if len(data) == 0 || out == nil {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode Nano response: %w", err)
	}
	return nil
}

func printJSON(value any) error {
	var data []byte
	var err error
	if raw, ok := value.(json.RawMessage); ok {
		var decoded any
		if err = json.Unmarshal(raw, &decoded); err == nil {
			data, err = json.MarshalIndent(decoded, "", "  ")
		}
	} else {
		data, err = json.MarshalIndent(value, "", "  ")
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(data))
	return err
}

func putIf(values map[string]any, key, value string) {
	if value != "" {
		values[key] = value
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: nano-agent [-base URL] <capabilities|list|start|run|send|inbox|outbox|message|ack|assign|resolve|status|heartbeat|answer|interrupt|close|detach|delete> [flags]")
}

func exitErr(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "nano-agent:", err)
	os.Exit(1)
}
