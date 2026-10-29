package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"time"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threads"
)

func fixtureTurnForAction(store *hostedstore.ExecutionStore, id, action string) (threads.TurnRecord, bool) {
	for _, record := range store.TurnsForHosted(id) {
		if record.ClientActionID == action {
			return record, true
		}
	}
	return threads.TurnRecord{}, false
}

func fixtureListSessions(store *hostedstore.ExecutionStore) []string {
	rows, err := store.Control().CanonicalDatabase().Query(`SELECT hosted_thread_id FROM hosted_execution_sessions`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			panic(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		panic(err)
	}
	return ids
}

func fixtureAttachmentRecords(store *hostedstore.AttachmentStore) []hostedstore.Attachment {
	records := store.DetachedExcept(nil)
	sort.Slice(records, func(i, j int) bool { return records[i].HostedThreadID < records[j].HostedThreadID })
	return records
}

func fixtureWaitTurn(store *hostedstore.ExecutionStore, ctx context.Context, id string, revision int64, wait time.Duration) (threads.TurnRecord, bool) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/operations/%s?afterRevision=%d&waitMs=%d", id, revision, wait.Milliseconds()), nil).WithContext(ctx)
	request.SetPathValue("operationId", id)
	fixtureOwners((&server{agentExecutions: store, Registry: newSessionRegistry()})).hostedViews().HandleOperationRead(response, request)
	if response.Code == http.StatusNotFound {
		return threads.TurnRecord{}, false
	}
	if response.Code != http.StatusOK {
		panic(response.Body.String())
	}
	return store.Turn(id)
}

func fixtureWaitChanges(store *hostedstore.ExecutionStore, ctx context.Context, bookmark string, wait time.Duration) struct{ Bookmark string } {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/operations/changes?since=%s&waitMs=%d", url.QueryEscape(bookmark), wait.Milliseconds()), nil).WithContext(ctx)
	(&hostedmodel.ViewService{Executions: store}).HandleOperationChanges(response, request)
	var result struct{ Bookmark string }
	if response.Code != http.StatusOK {
		panic(response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		panic(err)
	}
	return result
}

func fixturePermissionEventsAfter(store *hostedstore.MetadataStore, id string, after int64, limit int) (hostedstore.PermissionEventPage, error) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/hosted-threads/%s/permission-events?after=%d&limit=%d", id, after, limit), nil)
	(&hostedmodel.ViewService{Metadata: store}).HandlePermissionEvents(response, request, threads.SessionRecord{ID: id})
	if response.Code != http.StatusOK {
		return hostedstore.PermissionEventPage{}, fmt.Errorf("permission events: %s", response.Body.String())
	}
	var page hostedstore.PermissionEventPage
	err := json.Unmarshal(response.Body.Bytes(), &page)
	return page, err
}

// Fixture assignments may include stale targets to exercise projection repair.
// Seed their canonical evidence through the production metadata writer.
func fixtureAppendOrchestrationAssignment(store *hostedstore.ExecutionStore, id, action string, assignment *hostedstore.Assignment) (int64, error) {
	disposition := metadataDispositionConfirmed
	var value any
	if assignment == nil {
		disposition = metadataDispositionCleared
	} else {
		value = *assignment
	}
	event, err := store.Control().AppendEvent(metadataEvent{HostedThreadID: id, Field: "orchestrationAssignment", Value: value, Disposition: disposition, Source: postureSourceOperator, Freshness: metadataFreshnessLive, SourceEventKey: "assignment:" + id + ":" + action, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	return event.ID, err
}
