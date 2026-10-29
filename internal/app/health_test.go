package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	featuremail "github.com/dkroshow/nano/internal/features/mail"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
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
		agentExecutions: &hostedstore.ExecutionStore{},
		controlMetadata: &hostedstore.MetadataStore{},
		mail:            &featuremail.Store{},
	}
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
}
