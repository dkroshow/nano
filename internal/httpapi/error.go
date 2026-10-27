package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

type ErrorEnvelope struct {
	Error      Error    `json:"error"`
	Candidates []string `json:"candidates,omitempty"`
	Status     any      `json:"status,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteErrorEnvelope(w, status, ErrorEnvelope{Error: Error{Code: code, Message: strings.TrimSuffix(message, "\n")}})
}

func WriteErrorEnvelope(w http.ResponseWriter, status int, body ErrorEnvelope) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteHTTPError(w http.ResponseWriter, message string, status int) {
	WriteError(w, status, ErrorCode(status), message)
}

func WriteNotFound(w http.ResponseWriter) {
	WriteError(w, http.StatusNotFound, "not_found", "Not found")
}

func WriteJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func ErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusPreconditionFailed:
		return "precondition_failed"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusUnprocessableEntity:
		return "unprocessable_entity"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusNotImplemented:
		return "not_implemented"
	case http.StatusBadGateway:
		return "provider_error"
	case http.StatusServiceUnavailable:
		return "unavailable"
	case http.StatusInternalServerError:
		return "internal_error"
	default:
		return "http_error"
	}
}
