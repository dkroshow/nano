package usageclients

// usage_limits_grok.go — Grok's account-usage extension.
//
// Grok exposes billing through the unstable ACP extension `_x.ai/billing`.
// The probe deliberately opens a short-lived client to the already-supervised
// native leader, initializes ACP, asks for that one method, then kills and
// reaps only its child. It never creates or loads a session and never starts a
// model turn. The leader remains entirely outside Nano's lifecycle here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/usage"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const (
	grokBillingMethod  = "_x.ai/billing"
	grokWeeklyMinutes  = 7 * 24 * 60
	grokMonthlyMinutes = 30 * 24 * 60
)

var grokUsageTimeout = 20 * time.Second

// grokUsageClient is intentionally small so decoder and failure behavior can
// be tested without a real leader. The production constructor below still uses
// the same command, environment, and leader-socket plumbing as sessions.
type grokUsageClient interface {
	StartContext(context.Context) error
	Call(context.Context, string, any, any) error
	Close() error
}

var newGrokUsageClient = func(command []string, cwd string) grokUsageClient {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = cwd
	cmd.Env = sessions.GrokProcessEnv()
	client := protocol.NewClientCmd(cmd)
	client.JSONRPCVersion = sessions.GrokJSONRPCVersion
	return client
}

// FetchGrokUsage reads the subscription window through the real Grok leader.
// One deadline covers process start, ACP initialization, and billing. Closing
// the client also kills and reaps the short-lived stdio child.
func FetchGrokUsage(ctx context.Context, defaults config.SessionDefaults) *usage.ProviderSnapshot {
	out := &usage.ProviderSnapshot{Provider: "grok", Buckets: []usage.Bucket{}}
	request := sessions.ProviderRequest{Mode: "start", Cwd: defaults.CWD, GrokBin: sessions.ResolveGrokBin(defaults.GrokBin),
		GrokLeaderSocket: defaults.GrokLeaderSocket,
	}
	command := sessions.GrokCommand(request)
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return out.Fail("Grok executable is not configured", nil)
	}
	// An explicit socket override must already name the supervised leader. The
	// CLI otherwise waits for its own connection timeout, which is unsuitable
	// for a five-minute poll and obscures a simple configuration mistake.
	if socket := strings.TrimSpace(request.GrokLeaderSocket); socket != "" {
		if _, err := os.Stat(socket); err != nil {
			return out.Fail("Grok leader is unavailable", fmt.Errorf("stat leader socket: %w", err))
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, grokUsageTimeout)
	defer cancel()
	client := newGrokUsageClient(command, sessions.GrokProbeCwd(defaults.CWD))
	defer client.Close()
	if err := client.StartContext(callCtx); err != nil {
		return out.Fail(grokUsageFailure("start", err), grokUsageErrorDetail("start", err))
	}

	var initialize json.RawMessage
	if err := client.Call(callCtx, "initialize", map[string]any{
		"protocolVersion": sessions.GrokProtocolVersion, "clientCapabilities": map[string]any{},
	}, &initialize); err != nil {
		return out.Fail(grokUsageFailure("initialize", err), grokUsageErrorDetail("initialize", err))
	}

	var billing json.RawMessage
	if err := client.Call(callCtx, grokBillingMethod, map[string]any{}, &billing); err != nil {
		return out.Fail(grokUsageFailure("billing", err), grokUsageErrorDetail("billing", err))
	}
	return grokUsageFromBilling(billing, time.Now().UTC())
}

// grokUsageFromBilling keeps financial fields out of Nano. It reads only the
// subscription tier and the one included-credit usage window; prepaid balance,
// on-demand fields, and auto-top-up are intentionally ignored.
func grokUsageFromBilling(raw json.RawMessage, observedAt time.Time) *usage.ProviderSnapshot {
	out := &usage.ProviderSnapshot{Provider: "grok", Buckets: []usage.Bucket{}}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		return out.Fail("Grok returned an unreadable usage response", fmt.Errorf("decode billing response: %w", err))
	}
	if response == nil {
		return out.Fail("Grok returned an unreadable usage response", errors.New("billing response was not an object"))
	}
	out.PlanType = grokBillingString(response, "subscription_tier", "subscriptionTier")

	config := response
	if rawConfig, ok := response["config"]; ok {
		if err := json.Unmarshal(rawConfig, &config); err != nil || config == nil {
			return out.Fail("Grok returned an unreadable usage response", errors.New("billing config was not an object"))
		}
	}

	if rawPeriod, present := config["currentPeriod"]; present {
		period, err := grokBillingPeriod(rawPeriod)
		if err != nil {
			return out.Fail("Grok returned an unreadable usage response", err)
		}
		minutes, label, group, ok := grokPeriodWindow(period.Type, period.Start, period.End)
		if !ok {
			return out.Wait("Grok reports no subscription usage limit for this plan")
		}
		// Protobuf omits this zero-valued scalar. Grok 1.0.3's own client
		// treats the omission as 0%, and a present currentPeriod identifies
		// this as the modern subscription response rather than a no-limit one.
		percent, _, err := grokBillingNumber(config, "creditUsagePercent")
		if err != nil {
			return out.Fail("Grok returned an unreadable usage response", err)
		}
		resetAt, _ := time.Parse(time.RFC3339Nano, period.End)
		out.ObservedAt = observedAt
		out.Buckets = append(out.Buckets, usage.Bucket{
			Key: "grok:primary", Label: label, Group: group, UsedPercent: min(100, max(0, percent)),
			WindowMinutes: minutes, ResetsAt: resetAt.Unix(), Primary: true,
		})
		return out
	}

	used, usedPresent, usedErr := grokBillingNumber(config, "used")
	limit, limitPresent, limitErr := grokBillingNumber(config, "monthlyLimit")
	if usedErr != nil || limitErr != nil {
		return out.Fail("Grok returned an unreadable usage response", firstNonNil(usedErr, limitErr))
	}
	if usedPresent && limitPresent && limit > 0 && used >= 0 {
		out.ObservedAt = observedAt
		out.Buckets = append(out.Buckets, usage.Bucket{
			Key: "grok:primary", Label: "month", Group: "monthly",
			UsedPercent: min(100, used/limit*100), WindowMinutes: grokMonthlyMinutes,
			ResetsAt: grokBillingResetAt(config), Primary: true,
		})
		return out
	}
	return out.Wait("Grok reports no subscription usage limit for this plan")
}

type grokUsagePeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

func grokBillingPeriod(raw json.RawMessage) (grokUsagePeriod, error) {
	var period grokUsagePeriod
	if err := json.Unmarshal(raw, &period); err != nil {
		return period, fmt.Errorf("currentPeriod was not an object: %w", err)
	}
	if strings.TrimSpace(period.Type) == "" || strings.TrimSpace(period.End) == "" {
		return period, errors.New("currentPeriod omitted its type or end time")
	}
	end, err := time.Parse(time.RFC3339Nano, period.End)
	if err != nil {
		return period, fmt.Errorf("currentPeriod end was not a timestamp: %w", err)
	}
	period.End = end.UTC().Format(time.RFC3339Nano)
	if period.Start != "" {
		if start, err := time.Parse(time.RFC3339Nano, period.Start); err == nil {
			period.Start = start.UTC().Format(time.RFC3339Nano)
		} else {
			return period, fmt.Errorf("currentPeriod start was not a timestamp: %w", err)
		}
	}
	return period, nil
}

func grokPeriodWindow(kind, start, end string) (int, string, string, bool) {
	typeName := strings.ToUpper(strings.TrimSpace(kind))
	minutes, label, group := 0, "", ""
	switch {
	case strings.Contains(typeName, "WEEK"):
		minutes, label, group = grokWeeklyMinutes, "week", "weekly"
	case strings.Contains(typeName, "MONTH"):
		minutes, label, group = grokMonthlyMinutes, "month", "monthly"
	default:
		return 0, "", "", false
	}
	if start != "" {
		from, fromErr := time.Parse(time.RFC3339Nano, start)
		to, toErr := time.Parse(time.RFC3339Nano, end)
		if fromErr == nil && toErr == nil {
			if reported := int(to.Sub(from).Round(time.Minute).Minutes()); reported > 0 {
				minutes = reported
			}
		}
	}
	return minutes, label, group, true
}

func grokBillingString(values map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if raw, ok := values[key]; ok && json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return ""
}

// grokBillingNumber accepts both direct JSON numbers and protobuf scalar
// wrappers (`{"val": 42}`). An empty wrapper is protobuf's omitted scalar,
// so it is zero. Missing is distinct from malformed, which keeps a changed
// wire type from becoming a convincing zero-usage bar.
func grokBillingNumber(values map[string]json.RawMessage, key string) (float64, bool, error) {
	raw, present := values[key]
	if !present {
		return 0, false, nil
	}
	var value float64
	if strings.TrimSpace(string(raw)) != "null" && json.Unmarshal(raw, &value) == nil {
		return value, true, nil
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper != nil {
		if len(wrapper) == 0 {
			return 0, true, nil
		}
		if rawValue, ok := wrapper["val"]; ok {
			var value float64
			if strings.TrimSpace(string(rawValue)) != "null" && json.Unmarshal(rawValue, &value) == nil {
				return value, true, nil
			}
		}
	}
	return 0, true, fmt.Errorf("%s was not numeric", key)
}

func grokBillingResetAt(config map[string]json.RawMessage) int64 {
	for _, key := range []string{"billingPeriodEnd", "periodEnd", "resetsAt"} {
		var value string
		if raw, ok := config[key]; ok && json.Unmarshal(raw, &value) == nil {
			if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return at.Unix()
			}
		}
	}
	return 0
}

func grokUsageFailure(stage string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Grok usage request timed out"
	}
	var rpcErr *protocol.RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.Code == -32601 {
			return "This Grok version does not support account usage"
		}
		if grokUsageAuthText(rpcErr.Message) {
			return "Grok authentication failed — run grok login"
		}
	}
	if grokUsageMissingExecutable(err) {
		return "Grok executable is not available"
	}
	if grokUsageMissingWorkingDirectory(err) {
		return "Nano working directory is unavailable"
	}
	if stage == "start" || stage == "initialize" || grokUsageLeaderText(err.Error()) {
		return "Grok leader is unavailable"
	}
	if grokUsageAuthText(err.Error()) {
		return "Grok authentication failed — run grok login"
	}
	return "Grok did not answer the usage request"
}

// grokUsageMissingExecutable recognizes the error produced by cmd.Start for
// an absent absolute executable. It intentionally does not preflight paths:
// that would add a TOCTOU race and would not work for PATH-resolved binaries.
func grokUsageMissingExecutable(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "executable file not found") {
		return true
	}
	var pathErr *os.PathError
	return errors.As(err, &pathErr) && pathErr.Op == "fork/exec" && errors.Is(pathErr.Err, os.ErrNotExist)
}

func grokUsageMissingWorkingDirectory(err error) bool {
	var pathErr *os.PathError
	return errors.As(err, &pathErr) && pathErr.Op == "chdir" && errors.Is(pathErr.Err, os.ErrNotExist)
}

func grokUsageErrorDetail(stage string, err error) error {
	if err == nil {
		return nil
	}
	// RPCError.Error deliberately excludes its Data field. This keeps raw
	// billing response data out of ErrorDetail and the in-memory attempt log.
	return fmt.Errorf("%s %s: %v", grokBillingMethod, stage, err)
}

func grokUsageAuthText(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "auth") || strings.Contains(text, "login") || strings.Contains(text, "unauthorized") || strings.Contains(text, "token")
}

func grokUsageLeaderText(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "leader") || strings.Contains(text, "socket") || strings.Contains(text, "connection refused") || strings.Contains(text, "no such file")
}

func firstNonNil(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
