package hosted

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

const (
	bulkClosePreserveStars = "preserve"
	bulkCloseRemoveStars   = "unstar"
)

type bulkCloseMemberResult struct {
	HostedThreadID    string `json:"hostedThreadId"`
	Provider          string `json:"provider"`
	NativeThreadID    string `json:"nativeThreadId,omitempty"`
	PreviousStatus    string `json:"previousStatus"`
	FinalStatus       string `json:"finalStatus"`
	Working           bool   `json:"working"`
	PreviouslyStarred bool   `json:"previouslyStarred"`
	StarAction        string `json:"starAction"`
	CloseAction       string `json:"closeAction"`
	Error             string `json:"error,omitempty"`
}

type bulkCloseResponse struct {
	RootHostedThreadID string                  `json:"rootHostedThreadId"`
	StarredPolicy      string                  `json:"starredPolicy"`
	Complete           bool                    `json:"complete"`
	MemberCount        int                     `json:"memberCount"`
	WorkingCount       int                     `json:"workingCount"`
	StarredCount       int                     `json:"starredCount"`
	Members            []bulkCloseMemberResult `json:"members"`
}

type bulkClosePreviewMember struct {
	HostedThreadID string `json:"hostedThreadId"`
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	Status         string `json:"status"`
	Depth          int    `json:"depth"`
	Working        bool   `json:"working"`
	Starred        bool   `json:"starred"`
}

type bulkClosePreview struct {
	RootHostedThreadID string                   `json:"rootHostedThreadId"`
	MemberCount        int                      `json:"memberCount"`
	WorkingCount       int                      `json:"workingCount"`
	StarredCount       int                      `json:"starredCount"`
	Members            []bulkClosePreviewMember `json:"members"`
}

type relationshipTreeMember struct {
	record ownedthreads.SessionRecord
	depth  int
}

// currentRelationshipTree returns the same assignment-first hosted tree the
// sidebar renders. The mutable related thread wins when it still exists;
// otherwise the immutable invoker remains the fallback, matching
// sidebarParentKeys + resolveSidebarParent over the complete hosted graph.
// Callers hold relationshipMu so the returned scope cannot move underneath a
// mutation that acts on it.
func (s *Controller) currentRelationshipTree(rootID string) ([]relationshipTreeMember, error) {
	if s.Executions == nil || s.Metadata == nil || s.MetadataError != "" {
		return nil, fmt.Errorf("current relationship metadata unavailable")
	}
	allRecords := s.Executions.ListSessions()
	canonicalByID := make(map[string]ownedthreads.SessionRecord, len(allRecords))
	records := make([]ownedthreads.SessionRecord, 0, len(allRecords))
	// The catalog has one visible row per provider conversation. A resume can
	// replace that row's hosted owner while old lineage still names the retired
	// hosted id. Collapse each retired alias to the active native-id owner, just
	// as the sidebar overlay does, so a visible family is never split across the
	// old and replacement records.
	for _, record := range allRecords {
		canonical := record
		if record.Provider != "" && record.NativeThreadID != "" {
			if current, found := s.Executions.SessionForNative(record.Provider, record.NativeThreadID); found {
				canonical = current
			}
		}
		canonicalByID[record.ID] = canonical
		if canonical.ID == record.ID {
			records = append(records, record)
		}
	}
	root, ok := canonicalByID[rootID]
	if !ok {
		return nil, fmt.Errorf("hosted tree root does not exist")
	}

	children := make(map[string][]ownedthreads.SessionRecord)
	for _, record := range records {
		if record.ID == root.ID {
			continue
		}
		parentID := ""
		assignment, err := s.Metadata.CurrentOrchestrationAssignment(record.ID)
		if err != nil {
			return nil, fmt.Errorf("resolve current relationship for %s: %w", record.ID, err)
		}
		if assignment != nil && assignment.RelatedHostedThreadID != "" {
			if parent, exists := canonicalByID[assignment.RelatedHostedThreadID]; exists {
				parentID = parent.ID
			}
		}
		if parentID == "" {
			if record.SidebarPlacement == ownedthreads.SidebarPlacementStandalone {
				continue
			}
			parentID = record.InvokerHostedThreadID
		}
		if parent, exists := canonicalByID[parentID]; exists {
			parentID = parent.ID
		} else if record.InvokerProvider != "" && record.InvokerNativeThreadID != "" {
			if parent, found := s.Executions.SessionForNative(record.InvokerProvider, record.InvokerNativeThreadID); found {
				parentID = parent.ID
			}
		}
		if parentID != "" {
			children[parentID] = append(children[parentID], record)
		}
	}
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			if a, b := threadvalues.ParseTimestamp(children[parentID][i].CreatedAt), threadvalues.ParseTimestamp(children[parentID][j].CreatedAt); !a.Equal(b) {
				return a.Before(b)
			}
			return children[parentID][i].ID < children[parentID][j].ID
		})
	}

	tree := make([]relationshipTreeMember, 0)
	seen := map[string]bool{}
	var appendTree func(ownedthreads.SessionRecord, int)
	appendTree = func(record ownedthreads.SessionRecord, depth int) {
		if seen[record.ID] {
			return
		}
		seen[record.ID] = true
		tree = append(tree, relationshipTreeMember{record: record, depth: depth})
		for _, child := range children[record.ID] {
			appendTree(child, depth+1)
		}
	}
	appendTree(root, 0)
	return tree, nil
}

func (s *Controller) bulkCloseMemberWorking(record ownedthreads.SessionRecord) bool {
	live := s.Views().sessionForHosted(record.ID)
	if live == nil {
		return false
	}
	summary := live.Summary()
	return summary.TurnActive && !summary.Waiting
}

// HandleBulkClosePreview serves the bounded hosted relationship-tree close preview.
func (s *Controller) HandleBulkClosePreview(w http.ResponseWriter, root ownedthreads.SessionRecord) {
	if s.Stars == nil {
		httpapi.WriteHTTPError(w, "thread stars unavailable; bulk close cannot preview star policy", http.StatusServiceUnavailable)
		return
	}
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	tree, err := s.currentRelationshipTree(root.ID)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	preview := bulkClosePreview{
		RootHostedThreadID: tree[0].record.ID,
		MemberCount:        len(tree),
		Members:            make([]bulkClosePreviewMember, 0, len(tree)),
	}
	for _, member := range tree {
		record := member.record
		working := s.bulkCloseMemberWorking(record)
		starred := s.Stars.IsStarred(record.Provider, record.NativeThreadID)
		if working {
			preview.WorkingCount++
		}
		if starred {
			preview.StarredCount++
		}
		preview.Members = append(preview.Members, bulkClosePreviewMember{
			HostedThreadID: record.ID, Provider: record.Provider, NativeThreadID: record.NativeThreadID,
			Status: record.Status, Depth: member.depth, Working: working, Starred: starred,
		})
	}
	httpapi.WriteJSON(w, preview)
}

// HandleBulkClose closes a selected hosted relationship tree under one lock.
func (s *Controller) HandleBulkClose(w http.ResponseWriter, r *http.Request, root ownedthreads.SessionRecord) {
	var request struct {
		StarredPolicy string `json:"starredPolicy"`
	}
	body := http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		httpapi.WriteHTTPError(w, `body must be {"starredPolicy":"preserve"} or {"starredPolicy":"unstar"}`, http.StatusBadRequest)
		return
	}
	request.StarredPolicy = strings.TrimSpace(request.StarredPolicy)
	if request.StarredPolicy != bulkClosePreserveStars && request.StarredPolicy != bulkCloseRemoveStars {
		httpapi.WriteHTTPError(w, `starredPolicy must be "preserve" or "unstar"`, http.StatusBadRequest)
		return
	}
	if s.Stars == nil {
		httpapi.WriteHTTPError(w, "thread stars unavailable; bulk close cannot apply an explicit star policy", http.StatusServiceUnavailable)
		return
	}

	// Never call code that can re-enter this server while relationshipMu is held.
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	tree, err := s.currentRelationshipTree(root.ID)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	starred := make(map[string]bool, len(tree))
	toUnstar := make([]hostedstore.AttachmentKey, 0)
	for _, treeMember := range tree {
		member := treeMember.record
		if s.Stars.IsStarred(member.Provider, member.NativeThreadID) {
			starred[member.ID] = true
			toUnstar = append(toUnstar, hostedstore.AttachmentKey{Provider: member.Provider, NativeThreadID: member.NativeThreadID})
		}
	}
	if request.StarredPolicy == bulkCloseRemoveStars && len(toUnstar) > 0 {
		if err := s.Stars.UnstarMany(toUnstar); err != nil {
			httpapi.WriteHTTPError(w, "could not remove tree stars: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	response := bulkCloseResponse{
		RootHostedThreadID: tree[0].record.ID,
		StarredPolicy:      request.StarredPolicy,
		Complete:           true,
		MemberCount:        len(tree),
		StarredCount:       len(toUnstar),
		Members:            make([]bulkCloseMemberResult, 0, len(tree)),
	}
	for _, treeMember := range tree {
		member := treeMember.record
		result := bulkCloseMemberResult{
			HostedThreadID:    member.ID,
			Provider:          member.Provider,
			NativeThreadID:    member.NativeThreadID,
			PreviousStatus:    member.Status,
			FinalStatus:       member.Status,
			PreviouslyStarred: starred[member.ID],
			StarAction:        "none",
			CloseAction:       "closed",
		}
		if member.Status == hostedstore.StatusExited {
			result.CloseAction = "already_exited"
		}
		if result.PreviouslyStarred {
			if request.StarredPolicy == bulkCloseRemoveStars {
				result.StarAction = "unstarred"
			} else {
				result.StarAction = "preserved"
			}
		}
		result.Working = s.bulkCloseMemberWorking(member)
		if result.Working {
			response.WorkingCount++
		}

		var failures []string
		if err := s.closeHostedThreadForRecord(member); err != nil {
			failures = append(failures, err.Error())
		} else if err := s.dropQueuedTurns(member); err != nil {
			failures = append(failures, "drop queued prompts: "+err.Error())
		} else if err := s.Executions.UpdateSession(member.ID, "", "", hostedstore.StatusExited, "closed by bulk close"); err != nil {
			failures = append(failures, "record close: "+err.Error())
		}
		if current, found, readErr := s.Executions.SessionFromCanonical(member.ID); readErr != nil {
			failures = append(failures, "read final status: "+readErr.Error())
		} else if found {
			result.FinalStatus = current.Status
		}
		if len(failures) > 0 || result.FinalStatus != hostedstore.StatusExited {
			response.Complete = false
			result.CloseAction = "failed"
			result.Error = strings.Join(failures, "; ")
			if result.Error == "" {
				result.Error = "hosted record did not reach exited status"
			}
		}
		response.Members = append(response.Members, result)
	}

	s.PublishRelationshipChange()
	status := http.StatusOK
	if !response.Complete {
		status = http.StatusMultiStatus
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
