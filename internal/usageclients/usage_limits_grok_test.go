package usageclients

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/sessions"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const grokWeeklyBillingFixture = `{
  "subscription_tier": "X Premium+",
  "config": {
    "creditUsagePercent": 17.5,
    "currentPeriod": {
      "type": "USAGE_PERIOD_TYPE_WEEKLY",
      "start": "2026-08-10T13:14:49.293508+00:00",
      "end": "2026-08-17T13:14:49.293508+00:00"
    }
  }
}`

func TestGrokUsageFromBillingModernWeekly(t *testing.T) {
	got := grokUsageFromBilling(json.RawMessage(grokWeeklyBillingFixture), time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if got.Error != "" || got.Notice != "" || got.PlanType != "X Premium+" {
		t.Fatalf("snapshot = %+v", got)
	}
	if len(got.Buckets) != 1 {
		t.Fatalf("buckets = %+v", got.Buckets)
	}
	b := got.Buckets[0]
	if b.Key != "grok:primary" || b.Label != "week" || b.Group != "weekly" ||
		b.UsedPercent != 17.5 || b.WindowMinutes != grokWeeklyMinutes || !b.Primary {
		t.Errorf("bucket = %+v", b)
	}
	if got := time.Unix(b.ResetsAt, 0).UTC().Format(time.RFC3339); got != "2026-08-17T13:14:49Z" {
		t.Errorf("reset = %s", got)
	}
}

func TestGrokUsageModernOmittedPercentIsZero(t *testing.T) {
	raw := strings.Replace(grokWeeklyBillingFixture, `"creditUsagePercent": 17.5,`, "", 1)
	got := grokUsageFromBilling(json.RawMessage(raw), time.Now().UTC())
	if got.Error != "" || got.Notice != "" || len(got.Buckets) != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
	if got.Buckets[0].UsedPercent != 0 {
		t.Errorf("omitted modern percent = %v, want 0", got.Buckets[0].UsedPercent)
	}
}

func TestGrokUsageModernPercentIsClamped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		percent string
		want    float64
	}{
		{name: "negative", percent: "-4", want: 0},
		{name: "over one hundred", percent: "137.5", want: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(grokWeeklyBillingFixture, "17.5", tc.percent, 1)
			got := grokUsageFromBilling(json.RawMessage(raw), time.Now().UTC())
			if got.Error != "" || len(got.Buckets) != 1 || got.Buckets[0].UsedPercent != tc.want {
				t.Fatalf("snapshot = %+v, want %v%%", got, tc.want)
			}
		})
	}
}

func TestGrokUsageFromBillingMonthlyAndLegacy(t *testing.T) {
	monthly := grokUsageFromBilling(json.RawMessage(`{
	  "subscription_tier": "Premium",
	  "config": {"creditUsagePercent": 12, "currentPeriod": {
	    "type": "USAGE_PERIOD_TYPE_MONTHLY",
	    "start": "2026-08-01T00:00:00Z", "end": "2026-08-31T00:00:00Z"
	  }}
	}`), time.Now().UTC())
	if len(monthly.Buckets) != 1 || monthly.Buckets[0].Label != "month" ||
		monthly.Buckets[0].Group != "monthly" || monthly.Buckets[0].WindowMinutes != grokMonthlyMinutes {
		t.Errorf("monthly bucket = %+v", monthly.Buckets)
	}

	legacy := grokUsageFromBilling(json.RawMessage(`{
	  "subscription_tier": "Legacy",
	  "config": {
	    "used": {"val": 25}, "monthlyLimit": {"val": 100},
	    "billingPeriodEnd": "2026-09-01T00:00:00Z"
	  }
	}`), time.Now().UTC())
	if legacy.Error != "" || legacy.Notice != "" || len(legacy.Buckets) != 1 {
		t.Fatalf("legacy snapshot = %+v", legacy)
	}
	b := legacy.Buckets[0]
	if b.UsedPercent != 25 || b.Group != "monthly" || b.ResetsAt != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("legacy bucket = %+v", b)
	}
}

func TestGrokUsageLegacyEmptyScalarWrapperIsZero(t *testing.T) {
	got := grokUsageFromBilling(json.RawMessage(`{
	  "config": {"used": {}, "monthlyLimit": {"val": 100}}
	}`), time.Now().UTC())
	if got.Error != "" || got.Notice != "" || len(got.Buckets) != 1 || got.Buckets[0].UsedPercent != 0 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestGrokBillingNumberRejectsMalformedTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want float64
		err  bool
	}{
		{name: "empty protobuf wrapper", raw: `{}`, want: 0},
		{name: "array", raw: `[]`, err: true},
		{name: "string", raw: `"12"`, err: true},
		{name: "null", raw: `null`, err: true},
		{name: "non-numeric wrapper", raw: `{"val":"12"}`, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := grokBillingNumber(map[string]json.RawMessage{"used": json.RawMessage(tc.raw)}, "used")
			if !present || (err != nil) != tc.err || got != tc.want {
				t.Fatalf("number = %v, present = %v, err = %v", got, present, err)
			}
		})
	}
}

func TestGrokUsageNoLimitIsNoticeAndMalformedIsError(t *testing.T) {
	noLimit := grokUsageFromBilling(json.RawMessage(`{"subscription_tier":"No cap","config":{"monthlyLimit":{"val":0}}}`), time.Now().UTC())
	if noLimit.Error != "" || noLimit.Notice == "" || len(noLimit.Buckets) != 0 {
		t.Errorf("no-limit snapshot = %+v", noLimit)
	}
	malformed := grokUsageFromBilling(json.RawMessage(`{"config":{"currentPeriod":"weekly"}}`), time.Now().UTC())
	if malformed.Error == "" || len(malformed.Buckets) != 0 {
		t.Errorf("malformed snapshot = %+v", malformed)
	}
}

type fakeGrokUsageClient struct {
	calls  []string
	closed bool
	start  func(context.Context) error
	call   func(context.Context, string, any) error
}

func (c *fakeGrokUsageClient) StartContext(ctx context.Context) error {
	if c.start != nil {
		return c.start(ctx)
	}
	return nil
}

func (c *fakeGrokUsageClient) Call(ctx context.Context, method string, _ any, result any) error {
	c.calls = append(c.calls, method)
	if c.call != nil {
		if err := c.call(ctx, method, result); err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeGrokUsageClient) Close() error {
	c.closed = true
	return nil
}

func TestFetchGrokUsageUsesOnlyInitializeAndBilling(t *testing.T) {
	original := newGrokUsageClient
	defer func() { newGrokUsageClient = original }()
	var command []string
	var clientCwd string
	fake := &fakeGrokUsageClient{call: func(_ context.Context, method string, result any) error {
		if method == grokBillingMethod {
			*(result.(*json.RawMessage)) = json.RawMessage(grokWeeklyBillingFixture)
		}
		return nil
	}}
	newGrokUsageClient = func(got []string, cwd string) grokUsageClient {
		command = append([]string(nil), got...)
		clientCwd = cwd
		return fake
	}
	socket := filepath.Join(t.TempDir(), "isolated-grok.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	configuredCwd := t.TempDir()
	got := FetchGrokUsage(context.Background(), config.SessionDefaults{
		CWD: configuredCwd, GrokBin: "/opt/grok", GrokLeaderSocket: socket,
	})
	if got.Error != "" || !fake.closed || !reflect.DeepEqual(fake.calls, []string{"initialize", grokBillingMethod}) {
		t.Fatalf("snapshot=%+v calls=%v closed=%v", got, fake.calls, fake.closed)
	}
	want := []string{"/opt/grok", "--sandbox", "off", "agent", "--leader", "--leader-socket", socket, "stdio"}
	if !reflect.DeepEqual(command, want) {
		t.Errorf("command = %#v, want %#v", command, want)
	}
	if clientCwd != configuredCwd {
		t.Errorf("client cwd = %q, want configured cwd %q", clientCwd, configuredCwd)
	}
}

func TestFetchGrokUsageFallsBackFromMissingConfiguredCwd(t *testing.T) {
	original := newGrokUsageClient
	defer func() { newGrokUsageClient = original }()

	var clientCwd string
	fake := &fakeGrokUsageClient{call: func(_ context.Context, method string, result any) error {
		if method == grokBillingMethod {
			*(result.(*json.RawMessage)) = json.RawMessage(grokWeeklyBillingFixture)
		}
		return nil
	}}
	newGrokUsageClient = func(_ []string, cwd string) grokUsageClient {
		clientCwd = cwd
		return fake
	}

	missing := filepath.Join(t.TempDir(), "removed-release-checkout")
	got := FetchGrokUsage(context.Background(), config.SessionDefaults{CWD: missing, GrokBin: "/opt/grok"})
	if got.Error != "" || !fake.closed {
		t.Fatalf("snapshot=%+v closed=%v", got, fake.closed)
	}
	if clientCwd == missing {
		t.Fatalf("usage probe retained missing cwd %q", clientCwd)
	}
	if info, err := os.Stat(clientCwd); err != nil || !info.IsDir() {
		t.Fatalf("usage probe fallback cwd %q is not an existing directory: info=%v err=%v", clientCwd, info, err)
	}
}

func TestFetchGrokUsageClassifiesUnsupportedAndTimeoutAndCloses(t *testing.T) {
	originalFactory, originalTimeout := newGrokUsageClient, grokUsageTimeout
	defer func() {
		newGrokUsageClient = originalFactory
		grokUsageTimeout = originalTimeout
	}()

	unsupported := &fakeGrokUsageClient{call: func(_ context.Context, method string, _ any) error {
		if method == grokBillingMethod {
			return &protocol.RPCError{Code: -32601, Message: "Method not found", Data: json.RawMessage(`{"secret":"billing response"}`)}
		}
		return nil
	}}
	newGrokUsageClient = func([]string, string) grokUsageClient { return unsupported }
	got := FetchGrokUsage(context.Background(), config.SessionDefaults{GrokBin: "/opt/grok"})
	if got.Error != "This Grok version does not support account usage" || !unsupported.closed {
		t.Fatalf("unsupported = %+v closed=%v", got, unsupported.closed)
	}
	if strings.Contains(got.ErrorDetail, "billing response") {
		t.Errorf("ErrorDetail leaked RPC data: %q", got.ErrorDetail)
	}

	grokUsageTimeout = 10 * time.Millisecond
	timedOut := &fakeGrokUsageClient{call: func(ctx context.Context, method string, _ any) error {
		if method == grokBillingMethod {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	newGrokUsageClient = func([]string, string) grokUsageClient { return timedOut }
	got = FetchGrokUsage(context.Background(), config.SessionDefaults{GrokBin: "/opt/grok"})
	if got.Error != "Grok usage request timed out" || !timedOut.closed {
		t.Fatalf("timeout = %+v closed=%v", got, timedOut.closed)
	}
}

func TestFetchGrokUsageMissingLeaderSocketFailsFastAndReapsChild(t *testing.T) {
	bin := sessions.ResolveGrokBin("")
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("Grok binary unavailable: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "missing-leader.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	got := FetchGrokUsage(ctx, config.SessionDefaults{CWD: t.TempDir(), GrokBin: bin, GrokLeaderSocket: socket})
	if got.Error != "Grok leader is unavailable" {
		t.Fatalf("missing leader = %+v", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("missing socket took %s, want a fast failure", elapsed)
	}
	// The actual leader is never touched. This checks only for a child that
	// still carries this test's unique nonexistent socket path after the probe.
	time.Sleep(50 * time.Millisecond)
	if out, err := exec.Command("pgrep", "-f", socket).Output(); err == nil && len(out) > 0 {
		t.Errorf("usage probe left Grok child behind for %s: %s", socket, out)
	}
}

func TestFetchGrokUsageMissingAbsoluteExecutable(t *testing.T) {
	missingBin := filepath.Join(t.TempDir(), "missing-grok")
	got := FetchGrokUsage(context.Background(), config.SessionDefaults{
		CWD: t.TempDir(), GrokBin: missingBin,
	})
	if got.Error != "Grok executable is not available" {
		t.Fatalf("missing executable = %+v", got)
	}
}

func TestGrokUsageFailureTaxonomy(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		err   error
		want  string
	}{
		{"missing binary", "start", exec.ErrNotFound, "Grok executable is not available"},
		{"missing cwd", "start", &os.PathError{Op: "chdir", Path: "/removed-release-checkout", Err: os.ErrNotExist}, "Nano working directory is unavailable"},
		{"leader", "initialize", errors.New("connection refused"), "Grok leader is unavailable"},
		{"auth", "billing", errors.New("authentication required"), "Grok authentication failed — run grok login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokUsageFailure(tc.stage, tc.err); got != tc.want {
				t.Errorf("failure = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGrokUsageFailureClassifiesRealMissingWorkingDirectory(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = filepath.Join(t.TempDir(), "removed-release-checkout")
	err := cmd.Run()
	if got := grokUsageFailure("start", err); got != "Nano working directory is unavailable" {
		t.Fatalf("failure = %q for %T %v", got, err, err)
	}
}
