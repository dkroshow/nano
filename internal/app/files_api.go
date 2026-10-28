package app

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	filemodel "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/setup"
)

const (
	generalFileMaxBytes  = filemodel.MaxBytes
	fileEvidenceMaxRows  = 2000
	fileEvidenceMaxBytes = 4 << 20
)

type fileEvidenceCatalog interface {
	FindFileEvidenceBlobs(context.Context, string, string, int, int) ([]string, error)
}

func (s *server) handleGeneralFileRead(w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimSpace(r.URL.Query().Get("path"))
	path, resolvedBy, err := s.resolveGeneralFile(r.Context(), requested,
		strings.TrimSpace(r.URL.Query().Get("cwd")),
		strings.TrimSpace(r.URL.Query().Get("threadId")),
		strings.TrimSpace(r.URL.Query().Get("sessionId")))
	if err != nil {
		writeGeneralFileError(w, err)
		return
	}
	meta, content, inspectErr := filemodel.Inspect(path, setup.ProtectedProviderRoots())
	if inspectErr != nil {
		writeGeneralFileError(w, inspectErr)
		return
	}
	meta.RequestedPath = requested
	meta.ResolvedBy = resolvedBy
	meta.Content = string(content)
	meta.Encoding = "utf-8"
	meta.LineCount = strings.Count(meta.Content, "\n")
	if len(content) > 0 && !strings.HasSuffix(meta.Content, "\n") {
		meta.LineCount++
	}
	writeGeneralFileJSON(w, http.StatusOK, meta)
}

func (s *server) handleGeneralFileMtime(w http.ResponseWriter, r *http.Request) {
	path, err := filemodel.CanonicalExistingAbsolute(strings.TrimSpace(r.URL.Query().Get("path")))
	if err != nil {
		writeGeneralFileError(w, err)
		return
	}
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		writeGeneralFileError(w, &filemodel.Error{Code: "not_found", Message: "File not found", Status: http.StatusNotFound})
		return
	}
	writeGeneralFileJSON(w, http.StatusOK, map[string]any{"path": path, "mtime": filemodel.ModificationTime(info)})
}

func (s *server) handleGeneralFileWrite(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path    string   `json:"path"`
		Content string   `json:"content"`
		Mtime   *float64 `json:"mtime"`
	}
	// JSON escaping can nearly double a valid text buffer, so limit decoded
	// content separately from the transport envelope.
	body := http.MaxBytesReader(w, r.Body, 2*generalFileMaxBytes+(64<<10))
	if err := json.NewDecoder(body).Decode(&request); err != nil || strings.TrimSpace(request.Path) == "" {
		writeGeneralFileError(w, &filemodel.Error{Code: "invalid_path", Message: "Body must contain path and content", Status: http.StatusBadRequest})
		return
	}
	if len(request.Content) > generalFileMaxBytes {
		writeGeneralFileError(w, &filemodel.Error{Code: "too_large", Message: "File exceeds 1 MiB", Status: http.StatusRequestEntityTooLarge})
		return
	}
	path, err := filemodel.CanonicalExistingAbsolute(strings.TrimSpace(request.Path))
	if err != nil {
		writeGeneralFileError(w, err)
		return
	}
	meta, _, inspectErr := filemodel.Inspect(path, setup.ProtectedProviderRoots())
	if inspectErr != nil {
		writeGeneralFileError(w, inspectErr)
		return
	}
	if !meta.Editable {
		writeGeneralFileError(w, &filemodel.Error{Code: "read_only", Message: meta.ReadOnlyReason, Status: http.StatusForbidden})
		return
	}
	if request.Mtime != nil && math.Abs(meta.Mtime-*request.Mtime) > filemodel.ModificationTimeTolerance {
		httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
			Code: "mtime_conflict", Message: "File changed on disk since it was read", Details: map[string]float64{"mtime": meta.Mtime},
		}})

		return
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		writeGeneralFileError(w, &filemodel.Error{Code: "write_failed", Message: "Cannot stat file", Status: http.StatusInternalServerError})
		return
	}
	if err := filemodel.WriteAtomic(path, []byte(request.Content), info.Mode().Perm()); err != nil {
		writeGeneralFileError(w, &filemodel.Error{Code: "write_failed", Message: err.Error(), Status: http.StatusInternalServerError})
		return
	}
	meta, _, inspectErr = filemodel.Inspect(path, setup.ProtectedProviderRoots())
	if inspectErr != nil {
		writeGeneralFileError(w, inspectErr)
		return
	}
	writeGeneralFileJSON(w, http.StatusOK, meta)
}

func (s *server) resolveGeneralFile(ctx context.Context, requested, cwd, threadID, sessionID string) (string, string, *filemodel.Error) {
	threadCWD := s.fileThreadCwd(ctx, threadID, sessionID)
	if threadID == "" {
		threadID = s.sessionCatalogThreadID(ctx, sessionID)
	}
	return filemodel.Resolve(requested, cwd, threadCWD, func(relative string) []string {
		blobs := s.liveFileEvidence(ctx, sessionID)
		if catalog, ok := s.catalog.(fileEvidenceCatalog); ok && threadID != "" {
			if durable, err := catalog.FindFileEvidenceBlobs(ctx, threadID, filepath.Base(relative), fileEvidenceMaxRows, fileEvidenceMaxBytes); err == nil {
				blobs = append(blobs, durable...)
			}
		}
		return blobs
	})
}

func (s *server) fileThreadCwd(ctx context.Context, threadID, sessionID string) string {
	if s == nil {
		return ""
	}
	if sessionID != "" {
		session := s.LookupSession(sessionID)
		if session != nil {
			if dir := filemodel.CleanDir(session.CWD()); dir != "" {
				return dir
			}
		}
	}
	if threadID == "" || s.catalog == nil {
		return ""
	}
	thread, err := s.catalog.LoadThread(ctx, threadID)
	if err != nil || thread.Metadata.CWD.Value == nil {
		return ""
	}
	return filemodel.CleanDir(*thread.Metadata.CWD.Value)
}

// sessionCatalogThreadID is the catalog thread of a session's current native
// thread, so a request that names only its session still searches its saved
// history.
func (s *server) sessionCatalogThreadID(ctx context.Context, sessionID string) string {
	resolver, ok := s.catalog.(threadReferenceResolver)
	if !ok || sessionID == "" {
		return ""
	}
	session := s.LookupSession(sessionID)
	if session == nil || session.ThreadID() == "" {
		return ""
	}
	threadID, err := resolver.ResolveThreadReference(ctx, session.ThreadID())
	if err != nil {
		return ""
	}
	return threadID
}

// liveFileEvidence is the text of the session's unsaved prompts and replies,
// newest first, within the evidence budget; saved history comes from the
// catalog.
func (s *server) liveFileEvidence(ctx context.Context, sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	session := s.LookupSession(sessionID)
	if session == nil || session.LiveTextService() == nil {
		return nil
	}
	texts := session.LiveTextService().CurrentTexts(ctx)
	var blobs []string
	total := 0
	for index := len(texts) - 1; index >= 0 && total < fileEvidenceMaxBytes; index-- {
		text := texts[index]
		if text == "" {
			continue
		}
		if remaining := fileEvidenceMaxBytes - total; len(text) > remaining {
			text = text[:remaining]
		}
		blobs = append(blobs, text)
		total += len(text)
	}
	return blobs
}

func writeGeneralFileError(w http.ResponseWriter, err *filemodel.Error) {
	status := err.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteErrorEnvelope(w, status, httpapi.ErrorEnvelope{
		Error:      httpapi.Error{Code: err.Code, Message: err.Message},
		Candidates: err.Candidates,
	})
}

func writeGeneralFileJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
