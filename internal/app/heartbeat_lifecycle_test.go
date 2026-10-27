package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExplicitCloseSelfPausesHeartbeat(t *testing.T) {
	s, _, _, _, target, _ := stage0Server(t)
	store := openTestHeartbeatStore(t)
	s.heartbeat = newHeartbeatService(s, store, true, nil)
	attachment, err := store.CreateAttachment(testHeartbeatInput(target.ID), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+target.ID+"/close", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("close = %d %s", recorder.Code, recorder.Body.String())
	}
	paused, err := store.Attachment(attachment.ID)
	if err != nil || paused.State != heartbeatStatePaused || paused.PauseReason != heartbeatTargetClosedReason {
		t.Fatalf("paused = %#v, %v", paused, err)
	}
	var eventType, detail string
	if err := store.DB.QueryRow(`SELECT event_type,detail FROM heartbeat_attachment_events WHERE attachment_id=? ORDER BY id DESC LIMIT 1`, attachment.ID).Scan(&eventType, &detail); err != nil {
		t.Fatal(err)
	}
	if eventType != "target_gone" || detail != heartbeatTargetClosedReason {
		t.Fatalf("audit = %q/%q", eventType, detail)
	}
}
