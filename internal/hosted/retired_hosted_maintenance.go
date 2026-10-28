package hosted

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/httpapi"
)

type retiredHostedReceipt struct {
	state.RetirementReceipt
	OK          bool                 `json:"ok"`
	Error       string               `json:"error,omitempty"`
	Resolutions []IdentityResolution `json:"resolutions"`
}

// HandleRetirement previews or applies retired hosted record maintenance.
func (s *Controller) HandleRetirement(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var wire struct {
		Apply               *bool                   `json:"apply"`
		OutboxDisposition   string                  `json:"outboxDisposition"`
		ExpectedFingerprint string                  `json:"expectedFingerprint,omitempty"`
		ExpectedCounts      *state.RetirementCounts `json:"expectedCounts,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Apply == nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "explicit apply and outboxDisposition are required")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "expected one JSON object")
		return
	}
	if wire.OutboxDisposition != "repoint" && wire.OutboxDisposition != "delete" || (*wire.Apply && wire.ExpectedFingerprint == "") {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "choose repoint or delete; apply also requires expectedFingerprint from preview")
		return
	}
	request := state.RetirementRequest{Apply: *wire.Apply, OutboxDisposition: wire.OutboxDisposition, ExpectedFingerprint: wire.ExpectedFingerprint, ExpectedCounts: wire.ExpectedCounts}
	receipt := s.retireHostedRecords(r.Context(), request)
	status := http.StatusOK
	if !receipt.OK {
		status = http.StatusConflict
	}
	writeCatalogJSON(w, status, receipt)
}

func (s *Controller) retireHostedRecords(ctx context.Context, request state.RetirementRequest) retiredHostedReceipt {
	result := retiredHostedReceipt{Resolutions: []IdentityResolution{}}
	control := s.controlStore()
	if control == nil || s.Executions == nil || s.Queue == nil || s.Executions.Control() != control || s.Queue.Database() != control.CanonicalDatabase() {
		result.Error = "canonical retirement stores are unavailable"
		return result
	}
	// Match the relationship -> execution -> database order of ordinary thread
	// mutations.
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	check := func(preview state.RetirementPreview) error {
		retired := map[string]bool{}
		for _, target := range preview.Records {
			retired[target.HostedThreadID] = true
		}
		// Runtime/attachment edges require a wider operation. Historical duplicate
		// cleanup must not detach a provider or rewrite an attachment file.
		return s.Runtime.CheckRetiredReferences(retired, func() error {
			if s.Attachments != nil {
				return s.Attachments.checkRetiredReferences(retired)
			}
			return nil
		})
	}
	verify := func(tx *sql.Tx, targets []state.RetiredHostedRecord) error {
		resolutions := []IdentityResolution{}
		for _, target := range targets {
			resolved, err := resolveCanonicalNativeIdentity(ctx, tx, target.NativeThreadID)
			if err != nil {
				return err
			}
			if resolved.MatchCount != 1 || resolved.HostedThreadID != target.OwnerHostedThreadID || resolved.NativeThreadID != target.NativeThreadID || resolved.Provider != target.Provider || resolved.NativeThreadIDProvisional {
				return fmt.Errorf("native identity %s did not resolve to its surviving owner", target.NativeThreadID)
			}
			// Names are a separate read-only overlay and do not enter the transaction.
			if name, ok := s.Views().Names.DurableName(resolved.Provider, resolved.NativeThreadID); ok {
				resolved.DisplayName = name
			}
			resolutions = append(resolutions, resolved)
		}
		result.Resolutions = resolutions
		return nil
	}
	receipt, err := s.Executions.retireHostedRecords(ctx, request, s.Queue.Path(), check, verify)
	result.RetirementReceipt = receipt
	if err != nil {
		result.Resolutions = []IdentityResolution{}
		result.Error = err.Error()
		return result
	}
	result.OK = true
	return result
}
