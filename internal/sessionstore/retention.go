package sessionstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type RetentionPolicy struct {
	MaxAge       time.Duration
	MaxResponses int64
	MaxBytes     int64
}

type PruneReport struct {
	Paths []string
	Bytes int64
}

type retainedEntry struct {
	path           string
	modified       time.Time
	bytes          int64
	isResponse     bool
	liveResponse   bool
	isSession      bool
	sessionID      string
	pinned         bool
	activelyPinned bool
}

// PinResponse protects an active response record from retention until release.
func (s *Store) PinResponse(id string) func() {
	return s.pinPath(s.responsePath(id), "", id)
}

// PinSession protects an active SDK session from retention until release.
func (s *Store) PinSession(sessionID string) func() {
	if sessionID == "" {
		return func() {}
	}
	return s.pinPath(filepath.Join(s.sessionsDir(), safeName(sessionID)), sessionID, "")
}

func (s *Store) pinPath(path, sessionID, responseID string) func() {
	s.mu.Lock()
	s.pins[path]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.pins[path] <= 1 {
				delete(s.pins, path)
			} else {
				s.pins[path]--
			}
			if responseID != "" && s.pins[path] == 0 {
				delete(s.deletedIDs, responseID)
			}
			if sessionID != "" {
				if _, orphan := s.orphanSessions[sessionID]; orphan {
					s.recordMaintenanceErrorLocked(s.cleanupSessionIfUnreferencedLocked(sessionID))
				}
			}
			s.mu.Unlock()
		})
	}
}

// SetRetentionPolicy configures automatic and explicit pruning limits.
func (s *Store) SetRetentionPolicy(policy RetentionPolicy) {
	s.mu.Lock()
	s.retention = policy
	s.mu.Unlock()
}

// ValidatePruneRoots verifies ownership markers without creating directories.
func (s *Store) ValidatePruneRoots() (bool, error) {
	roots, err := s.ValidateRoots()
	if err != nil {
		return false, err
	}
	present := false
	for _, root := range roots {
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false, err
		}
		present = true
		marker, readErr := os.ReadFile(filepath.Join(root, ownershipMarker))
		if readErr != nil || !validOwnershipMarker(marker) {
			return false, errors.New("refusing to prune unmarked storage root " + root)
		}
	}
	return present, nil
}

// Prune applies the configured policy. Dry runs report the exact deletion set.
func (s *Store) Prune(dryRun bool) (PruneReport, error) {
	if _, err := s.ValidatePruneRoots(); err != nil {
		return PruneReport{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(time.Now(), dryRun)
}

func (s *Store) pruneLocked(now time.Time, dryRun bool) (PruneReport, error) {
	entries, fixedBytes, err := s.retentionEntries()
	if err != nil {
		return PruneReport{}, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modified.Equal(entries[j].modified) {
			return entries[i].path < entries[j].path
		}
		return entries[i].modified.Before(entries[j].modified)
	})

	selected := map[string]bool{}
	selectEntry := func(entry retainedEntry) bool {
		if entry.pinned {
			return false
		}
		selected[entry.path] = true
		return true
	}
	if s.retention.MaxAge > 0 {
		cutoff := now.Add(-s.retention.MaxAge)
		for _, entry := range entries {
			if entry.modified.Before(cutoff) {
				selectEntry(entry)
			}
		}
	}
	if s.retention.MaxResponses > 0 {
		remaining := int64(0)
		for _, entry := range entries {
			if entry.isResponse && !selected[entry.path] {
				remaining++
			}
		}
		for _, entry := range entries {
			if remaining <= s.retention.MaxResponses {
				break
			}
			if entry.isResponse && !selected[entry.path] && selectEntry(entry) {
				remaining--
			}
		}
	}
	// A response deletion may orphan its SDK session. Plan those session
	// deletions now so dry-run and actual reports describe exactly the same set,
	// and so byte-quota planning credits the session bytes immediately instead
	// of over-pruning additional responses.
	planOrphanSessions := func() int64 {
		survivingSessions := map[string]bool{}
		orphanCandidates := map[string]bool{}
		for _, entry := range entries {
			if !entry.isResponse || entry.sessionID == "" {
				continue
			}
			sessionPath := filepath.Join(s.sessionsDir(), safeName(entry.sessionID))
			if entry.liveResponse && !selected[entry.path] {
				survivingSessions[sessionPath] = true
			} else if selected[entry.path] {
				orphanCandidates[sessionPath] = true
			}
		}
		var newlySelectedBytes int64
		for _, entry := range entries {
			if entry.isSession && orphanCandidates[entry.path] && !selected[entry.path] && !entry.activelyPinned && !survivingSessions[entry.path] {
				selected[entry.path] = true
				newlySelectedBytes += entry.bytes
			}
		}
		return newlySelectedBytes
	}
	planOrphanSessions()

	if s.retention.MaxBytes > 0 {
		total := fixedBytes
		for _, entry := range entries {
			if !selected[entry.path] {
				total += entry.bytes
			}
		}
		for _, entry := range entries {
			if total <= s.retention.MaxBytes {
				break
			}
			if selected[entry.path] || entry.pinned {
				continue
			}
			if selectEntry(entry) {
				total -= entry.bytes
				total -= planOrphanSessions()
			}
		}
	}

	report := PruneReport{}
	for _, entry := range entries {
		if !selected[entry.path] {
			continue
		}
		report.Paths = append(report.Paths, entry.path)
		report.Bytes += entry.bytes
		if !dryRun {
			if err := os.RemoveAll(entry.path); err != nil {
				return report, err
			}
		}
	}
	return report, nil
}

func (s *Store) retentionEntries() ([]retainedEntry, int64, error) {
	var entries []retainedEntry
	fixedBytes := int64(0)
	referencedSessions := map[string]bool{}

	responseEntries, err := os.ReadDir(s.responsesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	for _, entry := range responseEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.responsesDir(), entry.Name())
		item, err := retainedPathInfo(path)
		if err != nil {
			return nil, 0, err
		}
		record, err := readResponseRecordPath(path)
		if err != nil {
			return nil, 0, err
		}
		item.isResponse = true
		item.liveResponse = !record.Deleted
		item.sessionID = record.SDKSessionID
		item.pinned = s.pins[path] > 0
		item.activelyPinned = item.pinned
		if !record.Deleted && record.SDKSessionID != "" {
			referencedSessions[filepath.Join(s.sessionsDir(), safeName(record.SDKSessionID))] = true
		}
		entries = append(entries, item)
	}

	sessionEntries, err := os.ReadDir(s.sessionsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	for _, entry := range sessionEntries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(s.sessionsDir(), entry.Name())
		item, err := retainedPathInfo(path)
		if err != nil {
			return nil, 0, err
		}
		item.isSession = true
		item.activelyPinned = s.pins[path] > 0
		item.pinned = item.activelyPinned || referencedSessions[path]
		entries = append(entries, item)
	}

	cacheEntries, err := os.ReadDir(s.CacheDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	for _, entry := range cacheEntries {
		if entry.Name() == ownershipMarker {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return nil, 0, infoErr
			}
			fixedBytes += info.Size()
			continue
		}
		path := filepath.Join(s.CacheDir, entry.Name())
		item, err := retainedPathInfo(path)
		if err != nil {
			return nil, 0, err
		}
		item.pinned = s.pins[path] > 0
		entries = append(entries, item)
	}

	for _, root := range []string{s.DataDir, s.StateDir} {
		for _, name := range []string{ownershipMarker, "server.lock"} {
			info, err := os.Stat(filepath.Join(root, name))
			if err == nil {
				fixedBytes += info.Size()
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, 0, err
			}
		}
	}
	return entries, fixedBytes, nil
}

func (s *Store) cleanupSessionIfUnreferencedLocked(sessionID string) error {
	if sessionID == "" {
		return nil
	}
	sessionPath := filepath.Join(s.sessionsDir(), safeName(sessionID))
	if s.pins[sessionPath] > 0 {
		s.orphanSessions[sessionID] = struct{}{}
		return nil
	}
	entries, err := os.ReadDir(s.responsesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, err := readResponseRecordPath(filepath.Join(s.responsesDir(), entry.Name()))
		if err != nil {
			return err
		}
		if !record.Deleted && record.SDKSessionID == sessionID {
			delete(s.orphanSessions, sessionID)
			return nil
		}
	}
	if err := os.RemoveAll(sessionPath); err != nil {
		return err
	}
	delete(s.orphanSessions, sessionID)
	return nil
}

func readResponseRecordPath(path string) (ResponseRecord, error) {
	var record ResponseRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	if err := migrateResponseRecord(&record); err != nil {
		return record, err
	}
	return record, nil
}

func retainedPathInfo(path string) (retainedEntry, error) {
	entry := retainedEntry{path: path}
	err := filepath.WalkDir(path, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(entry.modified) {
			entry.modified = info.ModTime()
		}
		if !d.IsDir() {
			entry.bytes += info.Size()
		}
		return nil
	})
	return entry, err
}
