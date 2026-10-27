package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthRequiresHostedControl(t *testing.T) {
	s := &server{controlMetadataErr: "attachment migration failed"}
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "attachment migration failed") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestHealthAcceptsUsableHostedControl(t *testing.T) {
	s := &server{
		agentExecutions: &agentExecutionStore{},
		controlMetadata: &controlMetadataStore{},
		mail:            &agentMessageStore{},
	}
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
}
