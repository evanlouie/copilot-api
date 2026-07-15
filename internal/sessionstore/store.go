package sessionstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
)

type Store struct {
	DataDir  string
	StateDir string
	CacheDir string
	mu       sync.Mutex
}

const ownershipMarker = ".copilot-api-owned"

func New(dataDir, stateDir, cacheDir string) *Store {
	return &Store{DataDir: dataDir, StateDir: stateDir, CacheDir: cacheDir}
}

func (s *Store) Ensure() error {
	for _, dir := range []string{s.DataDir, s.StateDir, s.CacheDir, s.sessionsDir(), s.responsesDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure %s: %w", dir, err)
		}
	}
	for _, root := range []string{s.DataDir, s.StateDir, s.CacheDir} {
		if err := os.WriteFile(filepath.Join(root, ownershipMarker), []byte("copilot-api storage root\n"), 0o600); err != nil {
			return fmt.Errorf("write ownership marker in %s: %w", root, err)
		}
	}
	return nil
}

func (s *Store) LockPath() string     { return filepath.Join(s.StateDir, "server.lock") }
func (s *Store) sessionsDir() string  { return filepath.Join(s.DataDir, "sessions") }
func (s *Store) responsesDir() string { return filepath.Join(s.StateDir, "responses") }

func (s *Store) SaveSessionMetadata(sessionID string, meta SessionMetadata) error {
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
	Version              int                            `json:"version"`
	ID                   string                         `json:"id"`
	SDKSessionID         string                         `json:"sdk_session_id"`
	Model                string                         `json:"model"`
	Instructions         string                         `json:"instructions,omitempty"`
	CreatedAt            time.Time                      `json:"created_at"`
	UpdatedAt            time.Time                      `json:"updated_at"`
	Status               string                         `json:"status"`
	Stored               bool                           `json:"stored"`
	Deleted              bool                           `json:"deleted"`
	InputText            string                         `json:"input_text,omitempty"`
	Output               []openai.ResponseOutputItem    `json:"output"`
	OutputText           string                         `json:"output_text"`
	Usage                *openai.ResponseUsage          `json:"usage,omitempty"`
	PreviousResponseID   string                         `json:"previous_response_id,omitempty"`
	PendingBatchID       string                         `json:"pending_batch_id,omitempty"`
	RetainedPath         string                         `json:"retained_path,omitempty"`
	InstalledToolCatalog *openai.StoredToolCatalog      `json:"installed_tool_catalog,omitempty"`
	LoadedToolEvents     []openai.StoredLoadedToolEvent `json:"loaded_tool_events,omitempty"`
	ToolOutputs          []openai.StoredToolOutput      `json:"tool_outputs,omitempty"`
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
	// overwrite a response that the client has deleted.
	if !record.Deleted {
		if existing, err := s.loadResponseRecord(record.ID); err == nil && existing.Deleted {
			return ErrNotFound
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	record.UpdatedAt = time.Now().UTC()
	return writeJSON(s.responsePath(record.ID), record)
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
	record, err := s.LoadResponseForContinuation(id)
	if err != nil {
		return err
	}
	record.Deleted = true
	record.Stored = false
	record.Status = "deleted"
	return s.SaveResponse(record)
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
	if record.Version != ResponseRecordVersion {
		return record, fmt.Errorf("unsupported response record version %d for %q", record.Version, id)
	}
	return record, nil
}

func (s *Store) responsePath(id string) string {
	return filepath.Join(s.responsesDir(), safeName(id)+".json")
}

var ErrNotFound = errors.New("not found")

func (s *Store) Purge(dryRun bool) ([]string, error) {
	roots, err := validatedPurgeRoots([]string{s.DataDir, s.StateDir, s.CacheDir})
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
		if _, err := os.Stat(filepath.Join(root, ownershipMarker)); err != nil {
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
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	protected := map[string]bool{}
	for _, path := range []string{home, cwd, string(filepath.Separator)} {
		if path != "" {
			abs, _ := filepath.Abs(path)
			protected[filepath.Clean(abs)] = true
		}
	}
	cleaned := make([]string, 0, len(roots))
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve purge root %q: %w", root, err)
		}
		abs = filepath.Clean(abs)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = filepath.Clean(resolved)
		}
		if protected[abs] {
			return nil, fmt.Errorf("refusing to purge protected path %s", abs)
		}
		cleaned = append(cleaned, abs)
	}
	for i := range cleaned {
		for j := i + 1; j < len(cleaned); j++ {
			sep := string(filepath.Separator)
			if cleaned[i] == cleaned[j] || strings.HasPrefix(cleaned[i]+sep, cleaned[j]+sep) || strings.HasPrefix(cleaned[j]+sep, cleaned[i]+sep) {
				return nil, fmt.Errorf("refusing to purge overlapping roots %s and %s", cleaned[i], cleaned[j])
			}
		}
	}
	return cleaned, nil
}

func (s *Store) SizeSummary() (map[string]int64, error) {
	out := map[string]int64{}
	for _, root := range []string{s.DataDir, s.StateDir, s.CacheDir} {
		var total int64
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			return nil
		}); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		out[root] = total
	}
	return out, nil
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
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func safeName(id string) string {
	if id == "" {
		return "empty"
	}
	b := make([]rune, 0, len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b = append(b, r)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}
