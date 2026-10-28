package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/threads"
)

// CreationNameReader reads durable display names at the repair boundary.
type CreationNameReader interface {
	ThreadNameFor(context.Context, string, string) (string, bool)
}

// CreationNameRefReader supplies provenance when the catalog supports it.
type CreationNameRefReader interface {
	ThreadNameRefFor(context.Context, string, string) (*history.ThreadNameRef, bool)
}

// CreationNames owns publication and guarded repair of canonical creation names.
type CreationNames struct {
	Metadata   *MetadataStore
	Executions *ExecutionStore
	Queue      *prompts.LegacyQueue
	Reader     CreationNameReader
	Refs       CreationNameRefReader
	Projector  CreationNameProjector
	Refresh    func()
}

func (s *CreationNames) controlStore() *MetadataStore {
	if s.Metadata != nil {
		return s.Metadata
	}
	if s.Executions != nil {
		return s.Executions.Control()
	}
	return nil
}

type creationNameRepairHTTPReceipt struct {
	threads.CreationNameRepairReceipt
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// HandleRepair previews or applies a fingerprint-guarded creation-name repair.
func (s *CreationNames) HandleRepair(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var wire struct {
		Apply               *bool                                     `json:"apply"`
		ExpectedFingerprint string                                    `json:"expectedFingerprint,omitempty"`
		Records             []threads.CreationNameRepairRecord        `json:"records"`
		Unrecoverable       []threads.CreationNameUnrecoverableRecord `json:"unrecoverable,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Apply == nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "explicit apply is required")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "expected one JSON object")
		return
	}
	if *wire.Apply && wire.ExpectedFingerprint == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "apply requires expectedFingerprint from preview")
		return
	}
	receipt := s.repairCreationNames(r, threads.CreationNameRepairRequest{
		Apply: *wire.Apply, ExpectedFingerprint: wire.ExpectedFingerprint, Records: wire.Records, Unrecoverable: wire.Unrecoverable,
	})
	status := http.StatusOK
	if !receipt.OK {
		status = http.StatusConflict
	}
	writeCatalogJSON(w, status, receipt)
}

func (s *CreationNames) repairCreationNames(r *http.Request, request threads.CreationNameRepairRequest) creationNameRepairHTTPReceipt {
	result := creationNameRepairHTTPReceipt{}
	control := s.controlStore()
	if control == nil || s.Executions == nil || s.Queue == nil || s.Executions.Control() != control || s.Queue.Database() != control.CanonicalDatabase() {
		result.Error = "canonical creation-name repair stores are unavailable"
		return result
	}
	reader, ok := s.Reader, s.Reader != nil
	if !ok {
		result.Error = "durable display-name store is unavailable"
		return result
	}
	expected := make(map[string]string, len(request.Records))
	for _, record := range request.Records {
		expected[record.Provider+"\x00"+record.NativeThreadID] = record.Name
	}
	refReader, hasRefs := s.Refs, s.Refs != nil
	for _, identity := range appendCreationNameRepairIdentities(request) {
		if hasRefs {
			if ref, found := refReader.ThreadNameRefFor(r.Context(), identity.provider, identity.nativeThreadID); found && ref != nil && ref.Value != "" {
				want := expected[identity.provider+"\x00"+identity.nativeThreadID]
				if ref.Provenance == history.ThreadNameCreation && want != "" && ref.Value == want {
					continue
				}
				result.Error = "hosted thread " + identity.hostedThreadID + " already has a durable display name"
				return result
			}
			continue
		}
		if name, found := reader.ThreadNameFor(r.Context(), identity.provider, identity.nativeThreadID); found && name != "" {
			result.Error = "hosted thread " + identity.hostedThreadID + " already has a durable display name"
			return result
		}
	}
	publish := func(receipt threads.CreationNameRepairReceipt) error {
		projector, ok := s.Projector, s.Projector != nil
		if !ok {
			return errors.New("creation names were recorded but the derived names projection is unavailable")
		}
		projections := make([]CreationNameProjection, 0, len(receipt.Preview.Records))
		for _, record := range receipt.Preview.Records {
			projections = append(projections, CreationNameProjection{Provider: record.Provider, NativeThreadID: record.NativeThreadID, Name: record.Name})
		}
		if err := projector.PublishCreationNames(r.Context(), projections); err != nil {
			return fmt.Errorf("creation names were recorded but could not be published: %w", err)
		}
		s.Refresh()
		return nil
	}
	receipt, err := s.Executions.repairCreationNames(r.Context(), request, s.Queue.Path(), publish)
	result.CreationNameRepairReceipt = receipt
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.OK = true
	return result
}

type creationNameRepairIdentity struct{ hostedThreadID, provider, nativeThreadID string }

func appendCreationNameRepairIdentities(request threads.CreationNameRepairRequest) []creationNameRepairIdentity {
	identities := make([]creationNameRepairIdentity, 0, len(request.Records)+len(request.Unrecoverable))
	for _, record := range request.Records {
		identities = append(identities, creationNameRepairIdentity{record.HostedThreadID, record.Provider, record.NativeThreadID})
	}
	for _, record := range request.Unrecoverable {
		identities = append(identities, creationNameRepairIdentity{record.HostedThreadID, record.Provider, record.NativeThreadID})
	}
	return identities
}
