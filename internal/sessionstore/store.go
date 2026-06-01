package sessionstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
)

type Store struct {
	DataDir  string
	StateDir string
	CacheDir string
}

func New(dataDir, stateDir, cacheDir string) *Store {
	return &Store{DataDir: dataDir, StateDir: stateDir, CacheDir: cacheDir}
}

func (s *Store) Ensure() error {
	for _, dir := range []string{s.DataDir, s.StateDir, s.CacheDir, s.sessionsDir(), s.responsesDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		_ = os.Chmod(dir, 0o700)
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

type ResponseRecord struct {
	ID                 string                      `json:"id"`
	SDKSessionID       string                      `json:"sdk_session_id"`
	Model              string                      `json:"model"`
	Instructions       string                      `json:"instructions,omitempty"`
	CreatedAt          time.Time                   `json:"created_at"`
	UpdatedAt          time.Time                   `json:"updated_at"`
	Status             string                      `json:"status"`
	Stored             bool                        `json:"stored"`
	Deleted            bool                        `json:"deleted"`
	InputText          string                      `json:"input_text,omitempty"`
	Output             []openai.ResponseOutputItem `json:"output"`
	OutputText         string                      `json:"output_text"`
	Usage              *openai.ResponseUsage       `json:"usage,omitempty"`
	PreviousResponseID string                      `json:"previous_response_id,omitempty"`
	PendingBatchID     string                      `json:"pending_batch_id,omitempty"`
	RetainedPath       string                      `json:"retained_path,omitempty"`
}

func (s *Store) SaveResponse(record ResponseRecord) error {
	record.UpdatedAt = time.Now().UTC()
	return writeJSON(s.responsePath(record.ID), record)
}

func (s *Store) LoadResponse(id string) (ResponseRecord, error) {
	var record ResponseRecord
	b, err := os.ReadFile(s.responsePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return record, ErrNotFound
		}
		return record, err
	}
	if err := json.Unmarshal(b, &record); err != nil {
		return record, err
	}
	if record.Deleted || !record.Stored {
		return record, ErrNotFound
	}
	return record, nil
}

func (s *Store) LoadResponseForContinuation(id string) (ResponseRecord, error) {
	var record ResponseRecord
	b, err := os.ReadFile(s.responsePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return record, ErrNotFound
		}
		return record, err
	}
	if err := json.Unmarshal(b, &record); err != nil {
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

func (s *Store) responsePath(id string) string {
	return filepath.Join(s.responsesDir(), safeName(id)+".json")
}

var ErrNotFound = errors.New("not found")

func (s *Store) Purge(dryRun bool) ([]string, error) {
	roots := []string{s.DataDir, s.StateDir, s.CacheDir}
	var paths []string
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		paths = append(paths, root)
	}
	sort.Strings(paths)
	if dryRun {
		return paths, nil
	}
	for _, root := range roots {
		if err := os.RemoveAll(root); err != nil {
			return paths, err
		}
	}
	return paths, nil
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
