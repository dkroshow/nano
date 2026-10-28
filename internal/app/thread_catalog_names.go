package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// threadNamesSignature is the (existence, size, mtime) fingerprint used to skip
// re-reading an unchanged truth file each refresh cycle.
type threadNamesSignature struct {
	exists      bool
	size        int64
	modUnixNano int64
}

func mergeThreadNameSources(primary, creation []history.ThreadNameRecord) []history.ThreadNameRecord {
	merged := append([]history.ThreadNameRecord(nil), primary...)
	seen := make(map[string]bool, len(primary))
	for _, record := range primary {
		seen[string(record.Provider)+"\x00"+record.NativeThreadID] = true
	}
	for _, record := range creation {
		key := string(record.Provider) + "\x00" + record.NativeThreadID
		if !seen[key] {
			merged = append(merged, record)
			seen[key] = true
		}
	}
	return merged
}

func statThreadNames(path string) (threadNamesSignature, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return threadNamesSignature{}, nil
	}
	if err != nil {
		return threadNamesSignature{}, err
	}
	return threadNamesSignature{exists: true, size: info.Size(), modUnixNano: info.ModTime().UnixNano()}, nil
}

func (s *threadCatalogService) setNamesError(message string) {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	s.namesError = &message
}

// markNamesProcessed advances the change-detection signature and sets or clears
// the surfaced error (empty message clears it).
func (s *threadCatalogService) markNamesProcessed(signature threadNamesSignature, message string) {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	s.namesProcessed = true
	s.namesFileValid = message == ""
	s.namesSig = signature
	if message == "" {
		s.namesError = nil
		return
	}
	s.namesError = &message
}

func (s *threadCatalogService) namesErrorSnapshot() *string {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	if s.namesError == nil {
		return nil
	}
	message := *s.namesError
	return &message
}

// catalogRootScanStats is the per-root cost of one cycle, logged once per scan.
type catalogRootScanStats struct {
	Root      string
	Sources   int
	Changed   int
	Projected int
	Repairs   int
}

// RenameThread writes the user-assigned name for a nano thread id straight to
// the truth file — never the store — then requests a names refresh. The
// serialized catalog worker picks up the changed file signature, syncs the
// derived table, bumps catalog_version, and the SSE hub pushes it, without
// walking a provider root;
// this reuses the whole existing chain and keeps the version-read-after-sync
// safety argument intact. Any rename claims the record as provenance "manual".
// AssignThreadName writes a user-assigned name directly by the names store's
// own key (provider, native thread id). This is the spawn-time path: the New
// Window dialog names a session before any catalog row exists, so resolving a
// catalog id — RenameThread's first step — has nothing to resolve yet. Same
// truth file, same manual provenance, same refresh chain.
func (s *threadCatalogService) AssignThreadName(provider, nativeID, name string) error {
	validated, err := history.ValidateThreadName(name)
	if err != nil {
		return threadNameInvalidError{reason: err.Error()}
	}
	if provider == "" || nativeID == "" {
		return fmt.Errorf("assign thread name: provider and native id required")
	}
	changed, err := s.mutateThreadNames(history.ThreadNameMutation{
		Provider: history.Provider(provider), NativeThreadID: nativeID,
		Name: validated, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if changed {
		s.requestNames()
	}
	return nil
}

// AssignThreadNameIfEmpty is the pre-bind pending-rename path. The names-file
// lock makes the absence check and write one operation, so a concurrent or
// earlier operator rename always wins. Immutable creation names use the
// separate derived projection and never enter this truth file.
func (s *threadCatalogService) AssignThreadNameIfEmpty(provider, nativeID, name string) error {
	validated, err := history.ValidateThreadName(name)
	if err != nil {
		return threadNameInvalidError{reason: err.Error()}
	}
	if provider == "" || nativeID == "" {
		return fmt.Errorf("assign thread name: provider and native id required")
	}
	changed, err := s.mutateThreadNames(history.ThreadNameMutation{
		Provider: history.Provider(provider), NativeThreadID: nativeID,
		Name: validated, UpdatedAt: time.Now().UTC(), OnlyIfAbsent: true,
	})
	if err != nil {
		return err
	}
	if changed {
		s.requestNames()
	}
	return nil
}

// PublishCreationNames ensures the derived names table projects canonical
// creation facts, refreshing it synchronously when it does not yet. The
// supplied records are verification targets,
// not a second source: manual and imported truth may legitimately win, while a
// creation projection must match the canonical value exactly.
func (s *threadCatalogService) PublishCreationNames(ctx context.Context, expected []creationNameProjectionRecord) error {
	for _, record := range expected {
		if record.Provider == "" || record.NativeThreadID == "" {
			return errors.New("publish creation name: provider and native id required")
		}
		if _, err := history.ValidateThreadName(record.Name); err != nil {
			return fmt.Errorf("publish creation name: %w", err)
		}
	}
	// Every resume of a named thread publishes a name the previous run already
	// projected. A full refresh for each, in series under the one names lock,
	// held boot's Claude handshake slots, so an unchanged truth file whose
	// projection already holds needs none.
	if s.namesTruthUnchanged() && s.creationNamesProjected(ctx, expected) == nil {
		return nil
	}
	if err := s.refreshThreadNames(ctx); err != nil {
		return err
	}
	return s.creationNamesProjected(ctx, expected)
}

// namesTruthUnchanged reports whether the truth file is the valid one the last
// refresh already synced.
func (s *threadCatalogService) namesTruthUnchanged() bool {
	if s.namesPath == "" {
		return false
	}
	signature, err := statThreadNames(s.namesPath)
	if err != nil {
		return false
	}
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	return s.namesProcessed && s.namesFileValid && signature == s.namesSig
}

// creationNamesProjected checks that each expected creation name is the
// projected name, or that a manual or imported name outranks it.
func (s *threadCatalogService) creationNamesProjected(ctx context.Context, expected []creationNameProjectionRecord) error {
	for _, record := range expected {
		ref, ok := s.ThreadNameRefFor(ctx, record.Provider, record.NativeThreadID)
		if !ok || ref == nil {
			return fmt.Errorf("publish creation name: %s/%s was not projected", record.Provider, record.NativeThreadID)
		}
		switch ref.Provenance {
		case history.ThreadNameManual, history.ThreadNameHistoricalImport:
			continue
		case history.ThreadNameCreation:
			if ref.Value != record.Name {
				return fmt.Errorf("publish creation name: %s/%s projected %q, want %q", record.Provider, record.NativeThreadID, ref.Value, record.Name)
			}
		default:
			return fmt.Errorf("publish creation name: %s/%s has unknown provenance %q", record.Provider, record.NativeThreadID, ref.Provenance)
		}
	}
	return nil
}

// ThreadNameFor serves the durable name for one (provider, nativeID) — the
// live-session read half of the names surface (posture item 3): boot and
// click-resume hydrate the summary's display name from it. A read failure
// reads as unnamed; the catalog overlay remains the durable presentation.
func (s *threadCatalogService) ThreadNameFor(ctx context.Context, provider, nativeThreadID string) (string, bool) {
	ref, ok := s.ThreadNameRefFor(ctx, provider, nativeThreadID)
	if !ok {
		return "", false
	}
	return ref.Value, true
}

// ThreadNameRefFor keeps hosted metadata's read-time join faithful to the
// merged name sources: both value and provenance come from the catalog's
// derived name row.
func (s *threadCatalogService) ThreadNameRefFor(ctx context.Context, provider, nativeThreadID string) (*history.ThreadNameRef, bool) {
	ref, err := s.store.ThreadNameRefFor(ctx, history.Provider(provider), nativeThreadID)
	if err != nil {
		log.Printf("read thread name %s/%s: %v", provider, nativeThreadID, err)
		return nil, false
	}
	return ref, ref != nil
}

// NamedThreads returns catalog rows whose current user-assigned name equals
// name exactly. A name record with no indexed catalog row is still returned
// so identity resolve can follow native-id history after a Claude respawn.
func (s *threadCatalogService) NamedThreads(ctx context.Context, name string) ([]catalogThread, error) {
	records, err := s.store.ThreadNamesNamed(ctx, name)
	if err != nil {
		return nil, err
	}
	rows := make([]catalogThread, 0, len(records))
	for _, record := range records {
		page, err := s.ListThreads(ctx, catalogListQuery{
			ThreadID: record.NativeThreadID, IncludeSubagents: true, IncludeGuardians: true,
			IncludeProgrammatic: true, Archived: archivedIncludeFilter, Limit: maxThreadCatalogLimit,
		})
		if err != nil {
			return nil, err
		}
		found := false
		for _, item := range page.Items {
			if item.Provider == string(record.Provider) && item.NativeID == record.NativeThreadID {
				rows = append(rows, item)
				found = true
			}
		}
		if found {
			continue
		}
		nameRef := catalogThreadName{
			Value: record.Name, Provenance: string(record.Provenance), Evidence: threadNameEvidence,
		}
		row := catalogThread{
			Provider: string(record.Provider), NativeID: record.NativeThreadID, Name: &nameRef,
		}
		if record.UpdatedAt != nil {
			stamp := record.UpdatedAt.UTC().Format(time.RFC3339Nano)
			row.UpdatedAt = &stamp
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *threadCatalogService) RenameThread(ctx context.Context, id, name string) (catalogThreadName, error) {
	validated, err := history.ValidateThreadName(name)
	if err != nil {
		return catalogThreadName{}, threadNameInvalidError{reason: err.Error()}
	}
	provider, nativeID, err := s.resolveThreadKey(ctx, id)
	if err != nil {
		return catalogThreadName{}, err
	}
	changed, err := s.mutateThreadNames(history.ThreadNameMutation{
		Provider: provider, NativeThreadID: nativeID,
		Name: validated, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return catalogThreadName{}, err
	}
	if changed {
		s.requestNames()
	}
	return catalogThreadName{
		Value: validated, Provenance: string(history.ThreadNameManual), Evidence: threadNameEvidence,
	}, nil
}

// ClearThreadName removes the user-assigned name for a nano thread id from the
// truth file (regardless of provenance), then requests a names refresh through
// the same worker → sync → version-bump → SSE chain as RenameThread.
func (s *threadCatalogService) ClearThreadName(ctx context.Context, id string) error {
	provider, nativeID, err := s.resolveThreadKey(ctx, id)
	if err != nil {
		return err
	}
	changed, err := s.mutateThreadNames(history.ThreadNameMutation{
		Provider: provider, NativeThreadID: nativeID, Clear: true,
	})
	if err != nil {
		return err
	}
	if changed {
		s.requestNames()
	}
	return nil
}

// resolveThreadKey maps a nano thread id to its durable (provider, native id)
// through the catalog, which is the truth-file key. An unknown id is a 404.
func (s *threadCatalogService) resolveThreadKey(ctx context.Context, id string) (history.Provider, string, error) {
	detail, err := s.store.GetThread(ctx, id)
	if errors.Is(err, history.ErrThreadNotFound) {
		return "", "", errCatalogThreadNotFound
	}
	if err != nil {
		return "", "", err
	}
	return detail.Thread.Provider, detail.Thread.NativeID, nil
}

// mutateThreadNames serializes the read-modify-write of the whole truth-file
// snapshot under namesWriteMu: it reloads the file fresh (so external hand-edits
// are picked up), refuses to overwrite a malformed file, applies the single
// mutation, and writes atomically. It never touches the store.
func (s *threadCatalogService) mutateThreadNames(mutation history.ThreadNameMutation) (bool, error) {
	if s.namesPath == "" {
		return false, errNamesFileDisabled
	}
	s.namesWriteMu.Lock()
	defer s.namesWriteMu.Unlock()
	existing, err := os.ReadFile(s.namesPath)
	if errors.Is(err, os.ErrNotExist) {
		existing = nil
	} else if err != nil {
		return false, fmt.Errorf("read thread names file: %w", err)
	}
	result, changed, err := history.ApplyThreadNameSnapshot(existing, mutation)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := writeThreadNamesAtomic(s.namesPath, result); err != nil {
		return false, err
	}
	return true, nil
}

// writeThreadNamesAtomic writes data to a temp file in the same directory and
// renames it over the target, so a reader never sees a half-written snapshot.
func writeThreadNamesAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".thread-names.*.tmp")
	if err != nil {
		return fmt.Errorf("create thread names temp file: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write thread names temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close thread names temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace thread names file: %w", err)
	}
	committed = true
	return nil
}
