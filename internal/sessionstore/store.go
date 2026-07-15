package sessionstore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/safepath"
)

type Store struct {
	DataDir        string
	StateDir       string
	CacheDir       string
	mu             sync.Mutex
	retention      RetentionPolicy
	deletedIDs     map[string]struct{}
	pins           map[string]int
	orphanSessions map[string]struct{}
	maintenanceErr error
}

const (
	ownershipMarker              = ".copilot-api-owned"
	ownershipMarkerContent       = "copilot-api storage root v1\n"
	legacyOwnershipMarkerContent = "copilot-api storage root\n"
)

func New(dataDir, stateDir, cacheDir string) *Store {
	return &Store{DataDir: dataDir, StateDir: stateDir, CacheDir: cacheDir, deletedIDs: map[string]struct{}{}, pins: map[string]int{}, orphanSessions: map[string]struct{}{}}
}

// TakeMaintenanceError returns and clears the latest asynchronous retention or
// orphan-cleanup failure so readiness and shutdown can surface it once.
func (s *Store) TakeMaintenanceError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.maintenanceErr
	s.maintenanceErr = nil
	return err
}

func (s *Store) recordMaintenanceErrorLocked(err error) {
	if err != nil {
		s.maintenanceErr = err
	}
}

func (s *Store) Ensure() error {
	roots, err := s.ValidateRoots()
	if err != nil {
		return err
	}
	legacyNames := [][]string{{"sessions"}, {"responses", "server.lock"}, nil}
	for i, root := range roots {
		if err := ensureOwnedRoot(root, legacyNames[i]); err != nil {
			return err
		}
	}
	for _, dir := range []string{s.sessionsDir(), s.responsesDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure %s: %w", dir, err)
		}
	}
	return nil
}

func (s *Store) ValidateRoots() ([]string, error) {
	return validatedPurgeRoots([]string{s.DataDir, s.StateDir, s.CacheDir})
}

func ensureOwnedRoot(root string, allowedLegacyNames []string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create storage root %s: %w", root, err)
	}
	marker := filepath.Join(root, ownershipMarker)
	if content, err := os.ReadFile(marker); err == nil {
		if !validOwnershipMarker(content) {
			return fmt.Errorf("refusing storage root %s with an invalid ownership marker", root)
		}
		if err := os.Chmod(root, 0o700); err != nil {
			return fmt.Errorf("secure storage root %s: %w", root, err)
		}
		if string(content) == legacyOwnershipMarkerContent {
			if err := writeOwnershipMarker(root); err != nil {
				return err
			}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read ownership marker in %s: %w", root, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("inspect storage root %s: %w", root, err)
	}
	allowed := make(map[string]bool, len(allowedLegacyNames))
	for _, name := range allowedLegacyNames {
		allowed[name] = true
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("refusing to claim non-empty storage root %s (unexpected entry %s)", root, entry.Name())
		}
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("secure storage root %s: %w", root, err)
	}
	return writeOwnershipMarker(root)
}

func validOwnershipMarker(content []byte) bool {
	return string(content) == ownershipMarkerContent || string(content) == legacyOwnershipMarkerContent
}

func writeOwnershipMarker(root string) error {
	marker := filepath.Join(root, ownershipMarker)
	tmp, err := os.CreateTemp(root, ".copilot-api-marker-*.tmp")
	if err != nil {
		return fmt.Errorf("create ownership marker in %s: %w", root, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure ownership marker in %s: %w", root, err)
	}
	if _, err := tmp.WriteString(ownershipMarkerContent); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write ownership marker in %s: %w", root, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync ownership marker in %s: %w", root, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close ownership marker in %s: %w", root, err)
	}
	if err := os.Rename(tmpName, marker); err != nil {
		return fmt.Errorf("replace ownership marker in %s: %w", root, err)
	}
	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("sync ownership marker directory %s: %w", root, err)
	}
	return nil
}

func (s *Store) LockPath() string     { return filepath.Join(s.StateDir, "server.lock") }
func (s *Store) sessionsDir() string  { return filepath.Join(s.DataDir, "sessions") }
func (s *Store) responsesDir() string { return filepath.Join(s.StateDir, "responses") }

func (s *Store) SaveSessionMetadata(sessionID string, meta SessionMetadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.sessionsDir(), safeName(sessionID), "metadata.json")
	return writeJSON(path, meta)
}

type SessionMetadata struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	OpenAIID       string    `json:"openai_id,omitempty"`
	SDKSessionID   string    `json:"sdk_session_id"`
	Model          string    `json:"model"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	RetainedPath   string    `json:"retained_path"`
	FinishReason   string    `json:"finish_reason,omitempty"`
	PendingBatchID string    `json:"pending_batch_id,omitempty"`
}

const ResponseRecordVersion = 3

type ResponseRecord struct {
	Version              int                     `json:"version"`
	ID                   string                  `json:"id"`
	SDKSessionID         string                  `json:"sdk_session_id"`
	Model                string                  `json:"model"`
	Instructions         string                  `json:"instructions,omitempty"`
	CreatedAt            time.Time               `json:"created_at"`
	UpdatedAt            time.Time               `json:"updated_at"`
	Status               string                  `json:"status"`
	Stored               bool                    `json:"stored"`
	Deleted              bool                    `json:"deleted"`
	InputText            string                  `json:"input_text,omitempty"`
	Output               []ResponseOutputItem    `json:"output"`
	OutputText           string                  `json:"output_text"`
	Usage                *ResponseUsage          `json:"usage,omitempty"`
	PreviousResponseID   string                  `json:"previous_response_id,omitempty"`
	PendingBatchID       string                  `json:"pending_batch_id,omitempty"`
	RetainedPath         string                  `json:"retained_path,omitempty"`
	InstalledToolCatalog *StoredToolCatalog      `json:"installed_tool_catalog,omitempty"`
	LoadedToolEvents     []StoredLoadedToolEvent `json:"loaded_tool_events,omitempty"`
	ToolOutputs          []StoredToolOutput      `json:"tool_outputs,omitempty"`
}

func (s *Store) SaveResponse(record ResponseRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.Version == 0 {
		record.Version = ResponseRecordVersion
	}
	if record.Version != ResponseRecordVersion {
		return fmt.Errorf("unsupported response record version %d", record.Version)
	}
	// Deletion is a tombstone: a late streaming callback must not resurrect or
	// overwrite a response that the client has deleted, even if retention has
	// already removed the on-disk tombstone during this process lifetime.
	markDeleted := record.Deleted
	if !record.Deleted {
		if _, deleted := s.deletedIDs[record.ID]; deleted {
			return ErrNotFound
		}
		if existing, err := s.loadResponseRecord(record.ID); err == nil && existing.Deleted {
			if s.pins[s.responsePath(record.ID)] > 0 {
				s.deletedIDs[record.ID] = struct{}{}
			}
			return ErrNotFound
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	record.UpdatedAt = time.Now().UTC()
	if err := writeJSON(s.responsePath(record.ID), record); err != nil {
		return err
	}
	if markDeleted && s.pins[s.responsePath(record.ID)] > 0 {
		s.deletedIDs[record.ID] = struct{}{}
	}
	return nil
}

func (s *Store) LoadResponse(id string) (ResponseRecord, error) {
	record, err := s.loadResponseRecord(id)
	if err != nil {
		return record, err
	}
	if record.Deleted || !record.Stored {
		return record, ErrNotFound
	}
	return record, nil
}

func (s *Store) LoadResponseForContinuation(id string) (ResponseRecord, error) {
	record, err := s.loadResponseRecord(id)
	if err != nil {
		return record, err
	}
	if record.Deleted {
		return record, ErrNotFound
	}
	return record, nil
}

func (s *Store) DeleteResponse(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadResponseRecord(id)
	if err != nil || record.Deleted {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	tombstone := ResponseRecord{
		Version:      ResponseRecordVersion,
		ID:           record.ID,
		SDKSessionID: record.SDKSessionID,
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    time.Now().UTC(),
		Deleted:      true,
		Status:       "deleted",
	}
	if err := writeJSON(s.responsePath(id), tombstone); err != nil {
		return err
	}
	if s.pins[s.responsePath(id)] > 0 {
		s.deletedIDs[id] = struct{}{}
	}
	s.recordMaintenanceErrorLocked(s.cleanupSessionIfUnreferencedLocked(record.SDKSessionID))
	return nil
}

func (s *Store) loadResponseRecord(id string) (ResponseRecord, error) {
	var record ResponseRecord
	b, err := os.ReadFile(s.responsePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return record, ErrNotFound
		}
		return record, err
	}
	if err := json.Unmarshal(b, &record); err != nil {
		return record, fmt.Errorf("decode response record %q: %w", id, err)
	}
	if err := migrateResponseRecord(&record); err != nil {
		return record, fmt.Errorf("response record %q: %w", id, err)
	}
	return record, nil
}

func (s *Store) responsePath(id string) string {
	return filepath.Join(s.responsesDir(), safeName(id)+".json")
}

var ErrNotFound = errors.New("not found")

type UnsupportedRecordVersionError struct {
	Version int
}

func (e *UnsupportedRecordVersionError) Error() string {
	return fmt.Sprintf("unsupported response record version %d", e.Version)
}

func migrateResponseRecord(record *ResponseRecord) error {
	switch record.Version {
	case 0, 1, 2:
		// Records written before explicit versioning decode as version 0. Versions
		// 0-2 used field shapes retained by the v3 persistence DTO.
		if record.ID == "" {
			return fmt.Errorf("response record is missing id")
		}
		// Normalize missing lifecycle fields explicitly.
		if record.Status == "" {
			record.Status = "completed"
		}
		record.Version = ResponseRecordVersion
		return nil
	case ResponseRecordVersion:
		return nil
	default:
		return &UnsupportedRecordVersionError{Version: record.Version}
	}
}

func (s *Store) Purge(dryRun bool) ([]string, error) {
	roots, err := s.ValidateRoots()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		marker, err := os.ReadFile(filepath.Join(root, ownershipMarker))
		if err != nil || !validOwnershipMarker(marker) {
			return nil, fmt.Errorf("refusing to purge unmarked storage root %s", root)
		}
		paths = append(paths, root)
	}
	sort.Strings(paths)
	if dryRun {
		return paths, nil
	}
	for _, root := range paths {
		if err := os.RemoveAll(root); err != nil {
			return paths, err
		}
	}
	return paths, nil
}

func validatedPurgeRoots(roots []string) ([]string, error) {
	return safepath.ValidateApplicationRoots(roots)
}

func writeJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(dir, ".copilot-api-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("secure temporary file for %s: %w", path, err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temporary file for %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temporary file for %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync directory for %s: %w", path, err)
	}
	return nil
}

func safeName(id string) string {
	if id == "" {
		return "~"
	}
	if id != "." && id != ".." && !strings.HasSuffix(id, ".") && !windowsReservedName(id) {
		safe := true
		for _, r := range id {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			default:
				safe = false
			}
			if !safe {
				break
			}
		}
		if safe {
			return id
		}
	}
	return "~" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func windowsReservedName(name string) bool {
	base := strings.ToLower(strings.SplitN(name, ".", 2)[0])
	if base == "con" || base == "prn" || base == "aux" || base == "nul" {
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9'
}
