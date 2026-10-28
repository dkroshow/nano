package threadindex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/catalog"
	"github.com/dkroshow/nano/internal/history"
)

func (s *Service) ListThreads(ctx context.Context, query ListQuery) (Page, error) {
	params := catalogQueryParams(query)
	params.ThreadID = query.ThreadID
	for _, key := range query.Identities {
		params.Identities = append(params.Identities, history.ThreadIdentity{Provider: key.Provider, NativeThreadID: key.NativeThreadID})
	}
	params.Limit = query.Limit
	if query.Cursor != "" {
		cursor, err := DecodeThreadCatalogCursor(query.Cursor)
		if err != nil {
			return Page{}, err
		}
		params.After = cursor
	}
	page, err := s.store.ListThreads(ctx, params)
	if err != nil {
		return Page{}, err
	}
	result := Page{Items: make([]Thread, 0, len(page.Items)), CatalogVersion: CatalogVersion(page.CatalogVersion)}
	for _, item := range page.Items {
		result.Items = append(result.Items, MapThread(item, nil))
	}
	if page.NextCursor != nil {
		encoded, err := EncodeThreadCatalogCursor(page.NextCursor)
		if err != nil {
			return Page{}, err
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

func (s *Service) SearchConversationItems(ctx context.Context, query history.ConversationSearchQuery) (history.ConversationSearchPage, error) {
	return s.store.SearchConversationItems(ctx, query)
}

func (s *Service) ConversationSearchStatus(ctx context.Context) ([]history.ConversationSearchStatus, error) {
	return s.store.ConversationSearchStatus(ctx)
}

func (s *Service) LoadThread(ctx context.Context, id string) (Thread, error) {
	detail, err := s.store.GetThread(ctx, id)
	if errors.Is(err, history.ErrThreadNotFound) {
		return Thread{}, ErrCatalogThreadNotFound
	}
	if err != nil {
		return Thread{}, err
	}
	sources := make([]SourceDetail, 0, len(detail.Sources))
	for _, source := range detail.Sources {
		diagnostics := make([]string, 0, len(source.Diagnostics))
		for _, diagnostic := range source.Diagnostics {
			message := string(diagnostic.Kind)
			if diagnostic.Line > 0 {
				message += fmt.Sprintf(" at line %d", diagnostic.Line)
			}
			if diagnostic.Message != "" {
				message += ": " + diagnostic.Message
			}
			diagnostics = append(diagnostics, message)
		}
		sources = append(sources, SourceDetail{
			RootKind: string(source.RootKind), Path: source.Path, Generation: source.Generation,
			ParserVersion: source.ParserVersion, Status: string(source.Status), Archived: source.Archived,
			IdentityEvidence: string(source.IdentityEvidence), DiagnosticCount: source.DiagnosticSummary.Total,
			Diagnostics: diagnostics, Error: source.ErrorSummary, LastIndexedAt: formatCatalogTime(source.LastIndexedAt),
		})
	}
	thread := MapThread(detail.Thread, &sources)
	// Zero coverage drops the field rather than reporting "0s · 0 turns": a
	// thread whose provider never recorded a duration has an unknown total, not
	// a zero one (D2/D8). A query failure degrades the same way — the detail
	// response is worth more than the one field.
	if totals, err := s.store.ThreadTurnTotals(ctx, id); err != nil {
		log.Printf("thread turn totals %s: %v", id, err)
	} else if totals.Turns > 0 {
		thread.Took = &ThreadTook{DurationMS: totals.DurationMS, Turns: totals.Turns}
	}
	return thread, nil
}

func (s *Service) LoadThreadUsage(ctx context.Context, id string, afterOrdinal, limit int) (history.ThreadUsagePage, error) {
	page, err := s.store.ThreadTurnUsage(ctx, id, afterOrdinal, limit)
	if errors.Is(err, history.ErrThreadNotFound) {
		return history.ThreadUsagePage{}, ErrCatalogThreadNotFound
	}
	return page, err
}

func (s *Service) ListThreadUsageTotals(ctx context.Context, query history.ThreadUsageListQuery) (history.ThreadUsageListPage, error) {
	return s.store.ListThreadUsageTotals(ctx, query)
}

func (s *Service) ReadSavedTurnStatus(ctx context.Context, threadID, nativeTurnID string) (history.SavedTurnStatus, bool, error) {
	return s.store.ReadSavedTurnStatus(ctx, threadID, nativeTurnID)
}

func (s *Service) LoadCompletedTranscriptTurn(ctx context.Context, id, nativeTurnID string) (TranscriptResponse, error) {
	page, err := s.store.ReadCompletedTranscriptTurn(ctx, id, nativeTurnID)
	if errors.Is(err, history.ErrTranscriptChanged) {
		return TranscriptResponse{}, errTranscriptCursorStale
	}
	if err != nil {
		return TranscriptResponse{}, err
	}
	response := TranscriptResponse{ThreadID: id, Projection: TranscriptProjection{Key: page.Projection.Key, State: string(page.Projection.State), SourceStatus: string(page.Projection.SourceStatus), EvidenceAvailable: page.Projection.EvidenceAvailable}, Page: TranscriptPageInfo{StartSeq: page.StartSeq, EndSeq: page.EndSeq, SnapshotHeadSeq: page.SnapshotHeadSeq}}
	for _, item := range page.Items {
		response.Items = append(response.Items, TranscriptItem{ID: item.ID, NativeItemID: item.NativeItemID, TurnID: item.TurnKey, ProjectedSeq: item.ProjectedSeq, Type: string(item.Type), Role: string(item.Role), Text: item.Text, Phase: item.Phase, IsFinalAnswer: item.IsFinalAnswer, Detail: append(json.RawMessage(nil), item.Detail...), ContentTruncated: item.ContentTruncated})
	}
	for _, turn := range page.Turns {
		response.Turns = append(response.Turns, TranscriptTurn{ID: turn.Key, NativeTurnID: turn.NativeTurnID, Ordinal: turn.Ordinal, StartSeq: turn.StartSeq, EndSeq: turn.EndSeq, Status: string(turn.Status), FinalItemID: turn.FinalItemID, CompletionEvidence: string(turn.CompletionEvidence), FinalitySource: turn.FinalitySource, FinalityConfidence: turn.FinalityConfidence})
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return TranscriptResponse{}, err
	}
	if len(encoded) > MaxThreadTranscriptBytes {
		return TranscriptResponse{}, history.ErrCompletedTranscriptUnavailable
	}
	return response, nil
}

func (s *Service) LoadTranscript(ctx context.Context, id string, query TranscriptQuery) (TranscriptResponse, error) {
	responses, _, err := s.LoadTranscripts(ctx, id, []TranscriptQuery{query}, nil)
	if err != nil {
		return TranscriptResponse{}, err
	}
	return responses[0], nil
}

// LoadTranscripts reads several pages of one thread, and which saved rows
// carry the given exact keys, from one consistent snapshot.
func (s *Service) LoadTranscripts(ctx context.Context, id string, queries []TranscriptQuery, match []history.TranscriptMatch) ([]TranscriptResponse, map[string]string, error) {
	requests := make([]history.TranscriptReadRequest, len(queries))
	for i := range queries {
		request, err := transcriptReadRequest(id, &queries[i])
		if err != nil {
			return nil, nil, err
		}
		requests[i] = request
	}
	pages, matched, err := s.store.ReadTranscripts(ctx, id, requests, match)
	switch {
	case errors.Is(err, history.ErrThreadNotFound):
		return nil, nil, ErrCatalogThreadNotFound
	case errors.Is(err, history.ErrTranscriptChanged):
		return nil, nil, errTranscriptCursorStale
	case errors.Is(err, history.ErrInvalidTranscriptPage):
		return nil, nil, errInvalidTranscriptCursor
	case err != nil:
		return nil, nil, err
	}
	responses := make([]TranscriptResponse, len(pages))
	for i, page := range pages {
		if responses[i], err = transcriptResponseFor(id, queries[i], page); err != nil {
			return nil, nil, err
		}
	}
	return responses, matched, nil
}

// transcriptReadRequest is one page query as the store reads it. It fills in
// the default profile's hidden categories on the query itself.
func transcriptReadRequest(id string, query *TranscriptQuery) (history.TranscriptReadRequest, error) {
	if !query.HiddenSpecified && query.View == DefaultThreadTranscriptView && len(query.HiddenCategories) == 0 {
		query.HiddenCategories = append([]string(nil), enhancedHiddenCategories...)
	}
	hidden := make([]history.TranscriptCategory, 0, len(query.HiddenCategories))
	for _, category := range query.HiddenCategories {
		hidden = append(hidden, history.TranscriptCategory(category))
	}
	request := history.TranscriptReadRequest{
		ThreadID: id, Limit: query.Limit, MaxBytes: MaxThreadTranscriptBytes,
		HiddenCategories: hidden, BeforeNativeTurnID: query.BeforeNativeTurn,
		BeforeTurnOrdinal: query.BeforeTurn, TurnLimit: query.TurnLimit,
	}
	// The default Enhanced profile is the message rows themselves, whatever
	// their status, not a category filter: a failed message keeps its error
	// category. Every other profile filters by category.
	if query.View == DefaultThreadTranscriptView && slices.Equal(query.HiddenCategories, enhancedHiddenCategories) {
		request.MessagesOnly, request.HiddenCategories = true, nil
	}
	if query.Before != "" {
		cursor, err := decodeThreadTranscriptCursor(query.Before)
		if err != nil || cursor.ThreadID != id || cursor.View != query.View || cursor.Profile != threadTranscriptProfile ||
			cursor.HiddenCategories != strings.Join(query.HiddenCategories, ",") {
			return history.TranscriptReadRequest{}, errInvalidTranscriptCursor
		}
		request.ExpectedProjectionKey = cursor.ProjectionKey
		request.SnapshotHeadSeq = cursor.SnapshotHeadSeq
		request.BeforeSeq = cursor.ExclusiveBeforeSeq
	}
	return request, nil
}

func transcriptResponseFor(id string, query TranscriptQuery, page history.TranscriptPage) (TranscriptResponse, error) {
	response := TranscriptResponse{
		ThreadID: id, View: query.View, Taxonomy: threadTranscriptTaxonomy,
		HiddenCategories: append([]string{}, query.HiddenCategories...),
		Items:            make([]TranscriptItem, 0, len(page.Items)),
		Turns:            make([]TranscriptTurn, 0, len(page.Turns)),
		Page: TranscriptPageInfo{
			StartSeq: page.StartSeq, EndSeq: page.EndSeq, LatestSeq: page.LatestSeq,
			SnapshotHeadSeq: page.SnapshotHeadSeq, HasMoreBefore: page.HasMoreBefore,
			StartsMidTurn: page.StartsMidTurn,
		},
		Projection: TranscriptProjection{
			Key: page.Projection.Key, ParserVersion: page.Projection.ParserVersion,
			SchemaVersion: page.Projection.SchemaVersion, State: string(page.Projection.State),
			SourceStatus:      string(page.Projection.SourceStatus),
			LastIndexedAt:     formatCatalogTime(page.Projection.LastIndexedAt),
			EvidenceAvailable: page.Projection.EvidenceAvailable, ItemCount: page.Projection.ItemCount,
			ErrorSummary: page.Projection.ErrorSummary,
		},
	}
	// A backward cursor is a fixed historical snapshot. Keep volatile append
	// freshness out of that representation so the page and its ETag remain
	// reusable while the same projection grows. Source-health changes still
	// invalidate it through state/status/evidence fields.
	if query.Before != "" {
		response.Page.LatestSeq = page.SnapshotHeadSeq
		response.Projection.ItemCount = page.SnapshotHeadSeq
		response.Projection.LastIndexedAt = nil
	}
	futureFinalItems := make(map[string]bool)
	for _, turn := range page.Turns {
		if turn.CompletionSeq > page.SnapshotHeadSeq && turn.FinalItemID != "" {
			futureFinalItems[turn.FinalItemID] = true
		}
	}
	for _, item := range page.Items {
		response.Items = append(response.Items, TranscriptItem{
			ID: item.ID, OwnerItemID: item.SourceKey, ProjectedSeq: item.ProjectedSeq, TurnID: item.TurnKey,
			NativeItemID: item.NativeItemID,
			Type:         string(item.Type), Category: string(item.Category), Status: string(item.Status), Role: string(item.Role),
			Text: item.Text, ToolName: item.ToolName, CallID: item.CallID,
			Detail: append(json.RawMessage(nil), item.Detail...), CreatedAt: formatCatalogTime(item.CreatedAt),
			Phase: item.Phase, FilterTags: append([]string{}, item.FilterTags...),
			Synthetic: item.Synthetic, Sidechain: item.Sidechain,
			IsFinalAnswer: item.IsFinalAnswer && !futureFinalItems[item.ID],
			ContentRef:    item.ContentRef, ContentTruncated: item.ContentTruncated,
			OriginalBytes: item.OriginalBytes,
		})
	}
	for _, turn := range page.Turns {
		endSeq := turn.EndSeq
		if endSeq > page.SnapshotHeadSeq {
			endSeq = page.SnapshotHeadSeq
		}
		status, completionEvidence, finalItemID := string(turn.Status), string(turn.CompletionEvidence), turn.FinalItemID
		finalitySource, finalityConfidence, durationMS := turn.FinalitySource, turn.FinalityConfidence, turn.DurationMS
		completedAt := turn.CompletedAt
		if turn.CompletionSeq > page.SnapshotHeadSeq {
			status, completionEvidence, finalItemID = string(history.TurnInProgress), string(history.CompletionNone), ""
			finalitySource, finalityConfidence, durationMS = "", "", 0
			completedAt = nil
		}
		response.Turns = append(response.Turns, TranscriptTurn{
			ID: turn.Key, Ordinal: turn.Ordinal, StartSeq: turn.StartSeq, EndSeq: endSeq,
			Status: status, CompletionEvidence: completionEvidence,
			FinalItemID: finalItemID, StartedAt: formatCatalogTime(turn.StartedAt),
			CompletedAt:    formatCatalogTime(completedAt),
			FinalitySource: finalitySource, FinalityConfidence: finalityConfidence,
			DurationMS: durationMS, NativeTurnID: turn.NativeTurnID,
			Config: append(json.RawMessage(nil), turn.Config...),
		})
	}
	if err := finalizeTranscriptResponse(&response); err != nil {
		return TranscriptResponse{}, err
	}
	return response, nil
}

func (s *Service) LoadTranscriptBody(ctx context.Context, threadID, itemID string) (history.TranscriptRetainedBody, error) {
	body, err := s.store.LoadTranscriptBody(ctx, threadID, itemID)
	switch {
	case errors.Is(err, history.ErrThreadNotFound):
		return history.TranscriptRetainedBody{}, ErrCatalogThreadNotFound
	case errors.Is(err, history.ErrInvalidTranscriptPage):
		return history.TranscriptRetainedBody{}, errInvalidTranscriptCursor
	case errors.Is(err, history.ErrTranscriptBodyUnavailable):
		return history.TranscriptRetainedBody{}, history.ErrTranscriptBodyUnavailable
	case err != nil:
		return history.TranscriptRetainedBody{}, err
	default:
		return body, nil
	}
}

func (s *Service) LoadTranscriptEvidence(ctx context.Context, threadID, itemID string) (TranscriptEvidenceResponse, error) {
	ref, err := s.store.LoadTranscriptEvidenceRef(ctx, threadID, itemID)
	switch {
	case errors.Is(err, history.ErrThreadNotFound):
		return TranscriptEvidenceResponse{}, ErrCatalogThreadNotFound
	case errors.Is(err, history.ErrInvalidTranscriptPage):
		return TranscriptEvidenceResponse{}, errInvalidTranscriptCursor
	case errors.Is(err, history.ErrTranscriptEvidenceUnavailable):
		return TranscriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	case errors.Is(err, history.ErrTranscriptChanged):
		return TranscriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	case err != nil:
		return TranscriptEvidenceResponse{}, err
	}
	if ref.SourceStatus != history.SourcePresent || ref.SourceGeneration != ref.CurrentGeneration {
		return TranscriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	}
	file, err := os.Open(ref.SourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TranscriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
		}
		return TranscriptEvidenceResponse{}, fmt.Errorf("open transcript evidence: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return TranscriptEvidenceResponse{}, fmt.Errorf("stat transcript evidence: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (ref.Device > 0 && int64(stat.Dev) != ref.Device) ||
		(ref.Inode > 0 && int64(stat.Ino) != ref.Inode) || info.Size() < ref.CompleteOffset {
		return TranscriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	}
	var raw []byte
	if ref.HasByteRange {
		raw, err = readTranscriptEvidenceRange(ctx, file, ref)
	} else {
		raw, err = readTranscriptEvidenceLine(ctx, file, ref.SourceLine)
	}
	if err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return TranscriptEvidenceResponse{}, errTranscriptEvidenceLarge
		}
		return TranscriptEvidenceResponse{}, fmt.Errorf("read transcript evidence: %w", err)
	}
	if len(raw) == 0 {
		return TranscriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	}
	if len(raw) > MaxThreadEvidenceBytes {
		return TranscriptEvidenceResponse{}, errTranscriptEvidenceLarge
	}
	digest := sha256.Sum256(raw)
	digestText := fmt.Sprintf("%x", digest[:])
	if digestText != ref.SourceLineSHA256 {
		return TranscriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
	}
	if ref.SourceRowType != "" && json.Valid(raw) {
		var row map[string]json.RawMessage
		var nativeType string
		if json.Unmarshal(raw, &row) != nil || json.Unmarshal(row["type"], &nativeType) != nil || nativeType != ref.SourceRowType {
			return TranscriptEvidenceResponse{}, errTranscriptEvidenceUnavailable
		}
	}
	response := TranscriptEvidenceResponse{
		ThreadID: ref.ThreadID, ItemID: ref.ItemID, ProjectionKey: ref.ProjectionKey,
		Provider: string(ref.Provider), SourcePath: ref.SourcePath, SourceLine: ref.SourceLine,
		SourceRowType: ref.SourceRowType, SourcePayloadType: ref.SourcePayloadType,
		RawBytes: len(raw), SHA256: digestText, ValidJSON: json.Valid(raw), ValidUTF8: utf8.Valid(raw),
	}
	if response.ValidUTF8 {
		value := string(raw)
		response.Raw = &value
	} else {
		response.RawBase64 = base64.StdEncoding.EncodeToString(raw)
	}
	return response, nil
}

// Ranges include a JSONL terminator, while native row hashes exclude it.
// A document may include trailing whitespace in its hash, so test exact bytes
// first and remove only one CRLF/LF terminator when the digest requires it.
func readTranscriptEvidenceRange(ctx context.Context, reader io.ReaderAt, ref history.TranscriptEvidenceRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.ByteStart < 0 || ref.ByteLength <= 0 || ref.ByteStart > ref.CompleteOffset || ref.ByteLength > ref.CompleteOffset-ref.ByteStart {
		return nil, errTranscriptEvidenceUnavailable
	}
	if ref.ByteLength > int64(MaxThreadEvidenceBytes)+2 {
		return nil, errTranscriptEvidenceLarge
	}
	raw, err := io.ReadAll(io.NewSectionReader(reader, ref.ByteStart, ref.ByteLength))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.ByteLength {
		return nil, errTranscriptEvidenceUnavailable
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) == ref.SourceLineSHA256 {
		return raw, nil
	}
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
		if fmt.Sprintf("%x", sha256.Sum256(raw)) == ref.SourceLineSHA256 {
			return raw, nil
		}
	}
	return nil, errTranscriptEvidenceUnavailable
}

func readTranscriptEvidenceLine(ctx context.Context, reader io.Reader, wantedLine int) ([]byte, error) {
	if wantedLine < 1 {
		return nil, errTranscriptEvidenceUnavailable
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), MaxThreadEvidenceBytes+1)
	line := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !scanner.Scan() {
			break
		}
		line++
		if line == wantedLine {
			return append([]byte(nil), scanner.Bytes()...), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errTranscriptEvidenceUnavailable
}

func finalizeTranscriptResponse(response *TranscriptResponse) error {
	if response.Items == nil {
		response.Items = []TranscriptItem{}
	}
	if response.Turns == nil {
		response.Turns = []TranscriptTurn{}
	}
	for {
		if err := setTranscriptBeforeCursor(response); err != nil {
			return err
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		if len(encoded) <= MaxThreadTranscriptBytes {
			return nil
		}
		if len(response.Items) <= 1 {
			if len(response.Items) == 1 && response.Items[0].ContentRef != "" &&
				(response.Items[0].Text != "" || len(response.Items[0].Detail) != 0) {
				// The retained-body endpoint owns the full value. Keep the row and
				// its paging identity reachable even when its preview alone would
				// exceed the page budget.
				response.Items[0].Text = ""
				response.Items[0].Detail = nil
				response.Items[0].ContentTruncated = true
				continue
			}
			return errors.New("bounded transcript item exceeds response budget")
		}
		response.Items = response.Items[1:]
		response.Page.StartSeq = response.Items[0].ProjectedSeq
		response.Page.HasMoreBefore = true
		response.Page.StartsMidTurn = true
		response.Turns = transcriptTurnsForItems(response.Turns, response.Items)
	}
}

func setTranscriptBeforeCursor(response *TranscriptResponse) error {
	response.Page.NextBeforeCursor = nil
	if !response.Page.HasMoreBefore || len(response.Items) == 0 {
		return nil
	}
	if response.Projection.Key == "" || response.Page.StartSeq <= 0 || response.Page.SnapshotHeadSeq <= 0 {
		return errors.New("transcript page cannot encode its backward cursor")
	}
	encoded, err := encodeThreadTranscriptCursor(threadTranscriptCursor{
		Version: threadTranscriptCursorVersion, ThreadID: response.ThreadID,
		ProjectionKey: response.Projection.Key, View: response.View, Profile: threadTranscriptProfile,
		HiddenCategories: strings.Join(response.HiddenCategories, ","),
		SnapshotHeadSeq:  response.Page.SnapshotHeadSeq, ExclusiveBeforeSeq: response.Page.StartSeq,
	})
	if err != nil {
		return err
	}
	response.Page.NextBeforeCursor = &encoded
	return nil
}

func transcriptTurnsForItems(turns []TranscriptTurn, items []TranscriptItem) []TranscriptTurn {
	used := make(map[string]bool, len(items))
	for _, item := range items {
		if item.TurnID != "" {
			used[item.TurnID] = true
		}
	}
	filtered := make([]TranscriptTurn, 0, len(turns))
	for _, turn := range turns {
		if used[turn.ID] {
			filtered = append(filtered, turn)
		}
	}
	return filtered
}

// catalogQueryParams maps the filter portion of an API query onto store
// params: toggles, archived, and time bounds.
func catalogQueryParams(query ListQuery) history.ListThreadsParams {
	return history.ListThreadsParams{
		IncludeSubagents: query.IncludeSubagents, IncludeGuardians: query.IncludeGuardians,
		IncludeProgrammatic: query.IncludeProgrammatic,
		Archived:            history.ArchivedFilter(query.Archived),
		CreatedFrom:         query.CreatedFrom, CreatedTo: query.CreatedTo,
		UpdatedFrom: query.UpdatedFrom, UpdatedTo: query.UpdatedTo,
	}
}

// IndexStatus reports catalog readiness and pending transcript publication.
func (s *Service) IndexStatus(ctx context.Context) (IndexStatus, error) {
	status, err := s.store.IndexStatus(ctx)
	if err != nil {
		return IndexStatus{}, err
	}
	result := IndexStatus{
		State: string(status.State), CatalogVersion: CatalogVersion(status.CatalogVersion),
		Roots:                   make([]RootStatus, 0, len(status.Roots)),
		InteractionCounts:       InteractionCounts(status.InteractionCounts),
		TranscriptChangeWorkers: transcriptChangeWorkerCount,
	}
	result.TranscriptChangeBacklog, result.TranscriptChangeInFlight = s.transcriptChangeStatus()
	for _, root := range status.Roots {
		result.Roots = append(result.Roots, RootStatus{
			Provider: string(root.Root.Root.Provider), RootKind: string(root.Root.Root.Kind),
			State: string(root.State), SourceCount: root.SourceCount,
			LastCompletedAt: formatCatalogTime(root.LastCompletedAt), Error: root.Error,
		})
	}
	if namesError := s.namesErrorSnapshot(); namesError != nil {
		result.State = string(history.IndexDegraded)
		result.NamesError = namesError
	}
	return result, nil
}

// publishCatalogVersion is the single point where a service-triggered version
// bump reaches SSE subscribers. Store writes are serialized on the scan
// goroutine, so a simple monotonic guard suppresses the redundant re-emits that
// unchanged reconciliations would otherwise produce.
func (s *Service) publishCatalogVersion(version uint64) {
	s.publishCatalogChange(catalogChange{Version: version, Refresh: true})
}

func CatalogVersion(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

// MapThread converts stored catalog data to the saved read representation.
func MapThread(thread history.Thread, sources *[]SourceDetail) Thread {
	var name *ThreadName
	if thread.Name != nil {
		name = &ThreadName{
			Value: thread.Name.Value, Provenance: string(thread.Name.Provenance),
			Evidence: ThreadNameEvidence,
		}
	}
	return Thread{
		ID: thread.ID, Provider: string(thread.Provider), NativeID: thread.NativeID,
		Kind: string(thread.Kind), ParentNativeID: thread.ParentNativeID,
		ProviderVersion: thread.ProviderVersion,
		Name:            name, Title: thread.Title, Preview: thread.Preview,
		CreatedAt: formatCatalogTime(thread.CreatedAt), UpdatedAt: formatCatalogTime(thread.UpdatedAt),
		Archived: thread.Archived, Metadata: MapMetadata(thread.Metadata),
		Interaction: string(thread.Interaction),
		Source: SourceSummary{
			Status: string(thread.Source.Status), Conflict: thread.Source.Conflict,
			DiagnosticCount: thread.Source.DiagnosticCount,
			LastIndexedAt:   formatCatalogTimeValue(thread.Source.LastIndexedAt),
		},
		Sources: sources,
	}
}

// MapMetadata converts stored metadata to the saved read representation.
func MapMetadata(metadata history.Metadata) Metadata {
	return Metadata{
		CWD: mapCatalogValue(metadata.CWD), Model: mapCatalogValue(metadata.Model),
		Effort: mapCatalogValue(metadata.Effort), Priority: mapCatalogValue(metadata.Priority),
		Permission:       mapCatalogValue(metadata.Permission),
		HistoricalBranch: mapCatalogValue(metadata.HistoricalBranch),
		Origin:           mapCatalogValue(metadata.Origin),
	}
}

func mapCatalogValue(field history.MetadataField) Value {
	var evidence *string
	if field.Evidence != nil {
		value := string(*field.Evidence)
		evidence = &value
	}
	return Value{State: string(field.State), Value: field.Value, Evidence: evidence}
}

func formatCatalogTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func formatCatalogTimeValue(value *time.Time) string {
	formatted := formatCatalogTime(value)
	if formatted == nil {
		return ""
	}
	return *formatted
}

// The opaque cursor carries the updated_at of the page boundary (the list
// order is fixed updated-desc).
type threadCatalogCursor struct {
	V  *string `json:"v"`
	ID string  `json:"id"`
}

func EncodeThreadCatalogCursor(cursor *history.ThreadCursor) (string, error) {
	value := threadCatalogCursor{ID: cursor.ID, V: formatCatalogTime(cursor.UpdatedAt)}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func DecodeThreadCatalogCursor(encoded string) (*history.ThreadCursor, error) {
	if len(encoded) == 0 || len(encoded) > maxThreadCatalogCursorBytes {
		return nil, ErrInvalidThreadCatalogCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrInvalidThreadCatalogCursor
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var value threadCatalogCursor
	if err := decoder.Decode(&value); err != nil || strings.TrimSpace(value.ID) == "" {
		return nil, ErrInvalidThreadCatalogCursor
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidThreadCatalogCursor
	}
	cursor := &history.ThreadCursor{ID: value.ID}
	if value.V != nil {
		parsed, err := time.Parse(time.RFC3339Nano, *value.V)
		if err != nil {
			return nil, ErrInvalidThreadCatalogCursor
		}
		cursor.UpdatedAt = &parsed
	}
	return cursor, nil
}

type threadTranscriptCursor struct {
	Version            int    `json:"v"`
	ThreadID           string `json:"threadId"`
	ProjectionKey      string `json:"projectionKey"`
	View               string `json:"view"`
	Profile            string `json:"profile"`
	HiddenCategories   string `json:"hiddenCategories"`
	SnapshotHeadSeq    int64  `json:"snapshotHeadSeq"`
	ExclusiveBeforeSeq int64  `json:"beforeSeq"`
}

func encodeThreadTranscriptCursor(cursor threadTranscriptCursor) (string, error) {
	if err := validateThreadTranscriptCursor(cursor); err != nil {
		return "", err
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > maxThreadTranscriptCursorBytes {
		return "", errInvalidTranscriptCursor
	}
	return encoded, nil
}

func decodeThreadTranscriptCursor(encoded string) (threadTranscriptCursor, error) {
	if len(encoded) == 0 || len(encoded) > maxThreadTranscriptCursorBytes {
		return threadTranscriptCursor{}, errInvalidTranscriptCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return threadTranscriptCursor{}, errInvalidTranscriptCursor
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var cursor threadTranscriptCursor
	if err := decoder.Decode(&cursor); err != nil {
		return threadTranscriptCursor{}, errInvalidTranscriptCursor
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return threadTranscriptCursor{}, errInvalidTranscriptCursor
	}
	if err := validateThreadTranscriptCursor(cursor); err != nil {
		return threadTranscriptCursor{}, err
	}
	return cursor, nil
}

func validateThreadTranscriptCursor(cursor threadTranscriptCursor) error {
	if cursor.Version != threadTranscriptCursorVersion || strings.TrimSpace(cursor.ThreadID) == "" ||
		strings.TrimSpace(cursor.ProjectionKey) == "" || (cursor.View != DefaultThreadTranscriptView && cursor.View != DevThreadTranscriptView) ||
		cursor.Profile != threadTranscriptProfile || cursor.SnapshotHeadSeq <= 0 ||
		cursor.ExclusiveBeforeSeq <= 0 || cursor.ExclusiveBeforeSeq > cursor.SnapshotHeadSeq+1 {
		return errInvalidTranscriptCursor
	}
	var hidden []string
	if cursor.HiddenCategories != "" {
		hidden = strings.Split(cursor.HiddenCategories, ",")
	}
	canonical, err := canonicalTranscriptCategories(hidden)
	if err != nil || strings.Join(canonical, ",") != cursor.HiddenCategories {
		return errInvalidTranscriptCursor
	}
	return nil
}

// FleetCourt answers the fleet classifier's read: the message that decides
// whose court each thread's ball is in (fleet_status.go). Computed per request
// and never stored — fleet state is a read-time projection.
func (s *Service) FleetCourt(ctx context.Context, threadIDs []string) (map[string]catalog.FleetCourtMessage, error) {
	records, err := s.store.FleetCourtMessages(ctx, threadIDs)
	if err != nil {
		return nil, err
	}
	courts := make(map[string]catalog.FleetCourtMessage, len(records))
	for id, record := range records {
		court := catalog.FleetCourtMessage{
			ItemType: string(record.ItemType), Head: record.Head, Tail: record.Tail,
			Interrupted: record.Interrupted,
		}
		if record.LastActivity != nil {
			court.LastActivity = *record.LastActivity
		}
		courts[id] = court
	}
	return courts, nil
}

// ResolveThreadReference turns an agent-supplied reference — an exact nano or
// native id, or an unambiguous prefix of either — into the durable nano id.
// Ambiguity is an error carrying the candidates, never a silent pick.
func (s *Service) ResolveThreadReference(ctx context.Context, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", ErrCatalogThreadNotFound
	}
	matches, err := s.store.ResolveThreadReferences(ctx, reference, MaxThreadReferenceCandidates)
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", ErrCatalogThreadNotFound
	case 1:
		return matches[0].ID, nil
	}
	candidates := make([]string, 0, len(matches))
	for _, match := range matches {
		candidates = append(candidates, match.ID)
	}
	return "", &AmbiguousReferenceError{Candidates: candidates}
}

var _ TranscriptCatalog = (*Service)(nil)

var _ TranscriptEvidenceCatalog = (*Service)(nil)

var _ catalog.FleetCourtSource = (*Service)(nil)

var _ ReferenceResolver = (*Service)(nil)
