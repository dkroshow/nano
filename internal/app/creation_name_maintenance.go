package app

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/httpapi"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/threads"
)

type creationNameRepairHTTPReceipt struct {
	threads.CreationNameRepairReceipt
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *server) handleCreationNameRepair(w http.ResponseWriter, r *http.Request) {
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

func (s *server) repairCreationNames(r *http.Request, request threads.CreationNameRepairRequest) creationNameRepairHTTPReceipt {
	result := creationNameRepairHTTPReceipt{}
	control := s.controlStore()
	if control == nil || s.agentExecutions == nil || s.queuedTurns == nil || s.agentExecutions.control != control || s.queuedTurns.db != control.db {
		result.Error = "canonical creation-name repair stores are unavailable"
		return result
	}
	reader, ok := s.catalog.(threadNameReader)
	if !ok {
		result.Error = "durable display-name store is unavailable"
		return result
	}
	expected := make(map[string]string, len(request.Records))
	for _, record := range request.Records {
		expected[record.Provider+"\x00"+record.NativeThreadID] = record.Name
	}
	refReader, hasRefs := s.catalog.(threadNameRefReader)
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
	s.agentExecutions.mu.Lock()
	defer s.agentExecutions.mu.Unlock()
	backupPath := ""
	if request.Apply {
		parent := filepath.Join(filepath.Dir(s.queuedTurns.path), "threads-maintenance-backups")
		if err := os.MkdirAll(parent, 0700); err != nil {
			result.Error = err.Error()
			return result
		}
		dir, err := os.MkdirTemp(parent, "creation-names-")
		if err != nil {
			result.Error = err.Error()
			return result
		}
		backupPath = filepath.Join(dir, "nano_threads.sqlite")
	}
	receipt, err := threads.RepairCreationNames(r.Context(), control.db, request, backupPath)
	result.CreationNameRepairReceipt = receipt
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if receipt.Applied {
		for _, record := range receipt.Preview.Records {
			var revision int64
			if err := control.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, record.HostedThreadID).Scan(&revision); err == nil {
				control.publishChange(record.HostedThreadID, revision, threads.CreationNameField)
			}
		}
		projector, ok := s.catalog.(creationNameProjector)
		if !ok {
			result.Error = "creation names were recorded but the derived names projection is unavailable"
			return result
		}
		projections := make([]creationNameProjectionRecord, 0, len(receipt.Preview.Records))
		for _, record := range receipt.Preview.Records {
			projections = append(projections, creationNameProjectionRecord{
				Provider: record.Provider, NativeThreadID: record.NativeThreadID, Name: record.Name,
			})
		}
		if err := projector.PublishCreationNames(r.Context(), projections); err != nil {
			result.Error = "creation names were recorded but could not be published: " + err.Error()
			return result
		}
		s.notifyRuntimeRefresh()
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
