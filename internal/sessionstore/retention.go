package sessionstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

const (
	// pruneRetryBase is the first backoff applied to a path os.RemoveAll refuses
	// to delete. It matches the retention loop interval, so the first retry is
	// the next cycle.
	pruneRetryBase = time.Minute
	// pruneRetryMax caps the backoff. A quarantined path is excluded from
	// planning entirely, so a permanently undeletable path cannot keep being
	// re-selected ahead of everything else and starve the rest of the store.
	pruneRetryMax = time.Hour
)

type retainedEntry struct {
	path           string
	modified       time.Time
	bytes          int64
	isResponse     bool
	isSession      bool
	pinned         bool
	activelyPinned bool
}

// pruneBackoff quarantines a path whose deletion keeps failing.
type pruneBackoff struct {
	failures int
	retryAt  time.Time
}

// retentionScan is the lock-free view of the store a prune plans from. Every
// field is derived from ReadDir and Stat alone: no response body is opened, so
// scan cost is independent of how much text, tool output, or catalog data the
// records carry.
type retentionScan struct {
	entries    []retainedEntry
	fixedBytes int64
	// sessionOf maps a response record path to the SDK session directory it is a
	// live reference for, and sessionRefs counts those references per session.
	// Both come from the retention link index rather than from record contents.
	sessionOf   map[string]string
	sessionRefs map[string]int
	// skipped collects per-path failures that a later cycle can retry. They are
	// reported but never latch readiness off.
	skipped error
	// indexPresent reports whether the retention link index could be read. When
	// it could not, nothing is known about which sessions are still referenced,
	// so no session may be deleted.
	indexPresent bool
}

// protectsEverySession reports that the scan cannot distinguish a referenced
// session from an unreferenced one. Deleting a session whose responses are
// still stored leaves those records pointing at a directory that no longer
// exists, which makes the conversation permanently unresumable, so an
// unavailable index degrades to "prune responses only".
func (scan *retentionScan) protectsEverySession() bool { return !scan.indexPresent }

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
//
// The scan runs without s.mu so request persistence is not blocked behind it.
// The lock is taken to plan against the current pin table and again, per path,
// around the deletion itself.
//
// Session deletions are planned from the retention link index alone. If that
// index cannot be established, no session is deleted at all: see
// retentionScan.protectsEverySession.
func (s *Store) Prune(dryRun bool) (PruneReport, error) {
	if _, err := s.ValidatePruneRoots(); err != nil {
		// A storage root that no longer validates is not something a later cycle
		// recovers from on its own, so it belongs on /readyz.
		s.mu.Lock()
		s.recordMaintenanceErrorLocked(err)
		s.mu.Unlock()
		return PruneReport{}, err
	}
	return s.prune(time.Now(), dryRun)
}

func (s *Store) prune(now time.Time, dryRun bool) (PruneReport, error) {
	// The prune subcommand never runs Ensure, so this is where a store written
	// before the index existed gets one. It takes s.mu itself, so it has to run
	// before planning rather than inside it. A dry run builds the index too:
	// otherwise it would predict a deletion set the real run cannot produce.
	indexErr := s.ensureRetentionLinks()
	scan, err := s.scanRetention()
	if err != nil {
		// The scan only fails outright when a whole storage directory is
		// unreadable, which means the quota cannot be enforced at all.
		s.mu.Lock()
		s.recordMaintenanceErrorLocked(err)
		s.mu.Unlock()
		return PruneReport{}, errors.Join(indexErr, err)
	}
	if indexErr != nil {
		// ensureRetentionLinks is a no-op once the index exists, so a build
		// failure means there is no complete index to plan sessions from - even if
		// the scan happened to read a partial one.
		scan.indexPresent = false
	}
	plan := s.planPrune(now, &scan)
	report, deleteErr := s.applyPrune(now, plan, &scan, dryRun)
	// An unavailable index is retryable and does not stop the prune, so like
	// every other skippable it is returned to the caller without latching
	// readiness off.
	return report, errors.Join(indexErr, scan.skipped, deleteErr)
}

// planPrune selects the deletion set from the freshly scanned entries. It reads
// the pin table under s.mu so a pin acquired while the lock-free scan was
// running is still honoured.
func (s *Store) planPrune(now time.Time, scan *retentionScan) []retainedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := scan.entries
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modified.Equal(entries[j].modified) {
			return entries[i].path < entries[j].path
		}
		return entries[i].modified.Before(entries[j].modified)
	})

	present := make(map[string]bool, len(entries))
	sessionIndex := map[string]int{}
	for i := range entries {
		entry := &entries[i]
		present[entry.path] = true
		entry.activelyPinned = s.pins[entry.path] > 0
		// A session with at least one live response reference is off limits to
		// every quota; only losing that last reference makes it collectable. With
		// no index there are no known references, which is not the same as no
		// references, so every session is protected instead.
		entry.pinned = entry.activelyPinned || (entry.isSession && (scan.protectsEverySession() || scan.sessionRefs[entry.path] > 0))
		if entry.isSession {
			sessionIndex[entry.path] = i
		}
	}
	for path := range s.deferredPrunes {
		if !present[path] {
			delete(s.deferredPrunes, path)
		}
	}

	policy := s.retention
	refs := maps.Clone(scan.sessionRefs)
	selected := map[string]bool{}
	quarantined := func(path string) bool {
		backoff, ok := s.deferredPrunes[path]
		return ok && now.Before(backoff.retryAt)
	}
	// selectEntry marks one entry for deletion and reports the bytes that frees.
	// Deleting the last live response of an SDK session orphans that session, so
	// the cascade is applied here rather than by rescanning every entry: the
	// reference counts are maintained incrementally, which keeps planning linear
	// even when the byte quota selects most of the store.
	selectEntry := func(i int) (int64, bool) {
		entry := &entries[i]
		if entry.pinned || selected[entry.path] || quarantined(entry.path) {
			return 0, false
		}
		selected[entry.path] = true
		freed := entry.bytes
		sessionPath, referenced := scan.sessionOf[entry.path]
		if !referenced {
			return freed, true
		}
		refs[sessionPath]--
		if refs[sessionPath] > 0 {
			return freed, true
		}
		j, known := sessionIndex[sessionPath]
		if !known {
			return freed, true
		}
		session := &entries[j]
		if selected[session.path] || session.activelyPinned || quarantined(session.path) {
			return freed, true
		}
		selected[session.path] = true
		return freed + session.bytes, true
	}

	if policy.MaxAge > 0 {
		cutoff := now.Add(-policy.MaxAge)
		for i := range entries {
			if entries[i].modified.Before(cutoff) {
				selectEntry(i)
			}
		}
	}
	if policy.MaxResponses > 0 {
		remaining := int64(0)
		for i := range entries {
			if entries[i].isResponse && !selected[entries[i].path] {
				remaining++
			}
		}
		for i := range entries {
			if remaining <= policy.MaxResponses {
				break
			}
			if !entries[i].isResponse || selected[entries[i].path] {
				continue
			}
			if _, ok := selectEntry(i); ok {
				remaining--
			}
		}
	}
	if policy.MaxBytes > 0 {
		total := scan.fixedBytes
		for i := range entries {
			if !selected[entries[i].path] {
				total += entries[i].bytes
			}
		}
		for i := range entries {
			if total <= policy.MaxBytes {
				break
			}
			if selected[entries[i].path] {
				continue
			}
			if freed, ok := selectEntry(i); ok {
				total -= freed
			}
		}
	}

	plan := make([]retainedEntry, 0, len(selected))
	var sessions []retainedEntry
	for _, entry := range entries {
		if !selected[entry.path] {
			continue
		}
		// Sessions go last. removeRetainedLocked re-counts a session's live
		// references before deleting it, so the responses this plan is about to
		// delete have to have dropped their links first or the session survives
		// until the next cycle.
		if entry.isSession {
			sessions = append(sessions, entry)
			continue
		}
		plan = append(plan, entry)
	}
	return append(plan, sessions...)
}

func (s *Store) applyPrune(now time.Time, plan []retainedEntry, scan *retentionScan, dryRun bool) (PruneReport, error) {
	report := PruneReport{}
	if dryRun {
		for _, entry := range plan {
			report.Paths = append(report.Paths, entry.path)
			report.Bytes += entry.bytes
		}
		return report, nil
	}
	// One undeletable path used to abort the whole loop. Because the plan is
	// ordered oldest first, that path was re-selected every cycle and nothing
	// behind it was ever reclaimed again.
	var failures []error
	for _, entry := range plan {
		s.mu.Lock()
		removed, err := s.removeRetainedLocked(now, entry, scan)
		s.mu.Unlock()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !removed {
			continue
		}
		report.Paths = append(report.Paths, entry.path)
		report.Bytes += entry.bytes
	}
	return report, errors.Join(failures...)
}

// removeRetainedLocked deletes one planned path, re-validating under s.mu that
// it is still collectable.
//
// The scan ran without the lock, so everything it observed is a hint. pinPath
// increments s.pins under this same mutex, so a pin is either visible to the
// check below (and the path survives) or cannot be taken until os.RemoveAll has
// already returned - there is no interleaving in which a pin is acquired
// between the check and the deletion.
func (s *Store) removeRetainedLocked(now time.Time, entry retainedEntry, scan *retentionScan) (bool, error) {
	if s.pins[entry.path] > 0 {
		return false, nil
	}
	switch {
	case entry.isSession:
		if scan.protectsEverySession() {
			// Planning already excludes every session in this case; this covers the
			// index becoming unavailable between the scan and this deletion.
			return false, nil
		}
		// SaveResponse publishes a session's retention link before writing the
		// record, both under s.mu, so this count is authoritative here even
		// though the scan's copy was not.
		refs, err := s.liveSessionRefsLocked(filepath.Base(entry.path))
		if err != nil {
			return false, err
		}
		if refs > 0 {
			return false, nil
		}
	case entry.isResponse:
		info, err := os.Stat(entry.path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("stat %s: %w", entry.path, err)
		}
		// Rewritten since the scan observed it, so the age and quota decisions
		// that selected it no longer describe this file.
		if info.ModTime().After(entry.modified) {
			return false, nil
		}
	}
	if err := os.RemoveAll(entry.path); err != nil {
		s.deferPruneLocked(now, entry.path)
		return false, fmt.Errorf("remove %s: %w", entry.path, err)
	}
	delete(s.deferredPrunes, entry.path)
	if entry.isSession {
		if err := os.RemoveAll(s.sessionLinksDir(filepath.Base(entry.path))); err != nil {
			return true, fmt.Errorf("remove retention links for %s: %w", entry.path, err)
		}
		return true, nil
	}
	if sessionPath, referenced := scan.sessionOf[entry.path]; referenced {
		name := strings.TrimSuffix(filepath.Base(entry.path), ".json")
		if err := s.removeLinkLocked(filepath.Base(sessionPath), name); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (s *Store) deferPruneLocked(now time.Time, path string) {
	backoff := s.deferredPrunes[path]
	backoff.failures++
	delay := pruneRetryBase << min(backoff.failures-1, 16)
	if delay > pruneRetryMax || delay <= 0 {
		delay = pruneRetryMax
	}
	backoff.retryAt = now.Add(delay)
	s.deferredPrunes[path] = backoff
}

// scanRetention builds the prune input without holding s.mu and without reading
// a single response body. Errors confined to one path are collected in
// scan.skipped so one unreadable record cannot stop the store from being
// pruned; the returned error is reserved for failures that make planning
// impossible.
func (s *Store) scanRetention() (retentionScan, error) {
	scan := retentionScan{sessionOf: map[string]string{}, sessionRefs: map[string]int{}}
	var skipped []error

	responseNames := map[string]bool{}
	responseEntries, err := os.ReadDir(s.responsesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return scan, fmt.Errorf("read %s: %w", s.responsesDir(), err)
	}
	for _, entry := range responseEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.responsesDir(), entry.Name())
		item, found, err := retainedPathInfo(path, entry)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		if !found {
			continue
		}
		item.isResponse = true
		responseNames[strings.TrimSuffix(entry.Name(), ".json")] = true
		scan.entries = append(scan.entries, item)
	}

	// An index that cannot be listed is not an index reporting zero references.
	// scan.indexPresent stays false in that case and planning protects every
	// session; the response quotas below still run, because refusing to prune at
	// all would let the store grow without bound instead.
	linkDirs, err := os.ReadDir(s.linksDir())
	switch {
	case err == nil:
		scan.indexPresent = true
	case errors.Is(err, os.ErrNotExist):
		// Prune builds the index before scanning, so reaching here means the
		// responses directory itself is absent and the build declined to create
		// storage. Nothing is known about references either way.
	default:
		skipped = append(skipped, fmt.Errorf("read %s: %w", s.linksDir(), err))
	}
	for _, sessionDir := range linkDirs {
		if !sessionDir.IsDir() {
			continue
		}
		linkPath := filepath.Join(s.linksDir(), sessionDir.Name())
		refs, err := os.ReadDir(linkPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			skipped = append(skipped, fmt.Errorf("read %s: %w", linkPath, err))
			continue
		}
		sessionPath := filepath.Join(s.sessionsDir(), sessionDir.Name())
		for _, ref := range refs {
			// A link whose record is missing from this snapshot is either crash
			// residue or a SaveResponse caught mid-flight. Counting it keeps the
			// session alive for another cycle, which is the safe direction; the
			// deletion path cleans up the residue under s.mu.
			scan.sessionRefs[sessionPath]++
			if responseNames[ref.Name()] {
				scan.sessionOf[filepath.Join(s.responsesDir(), ref.Name()+".json")] = sessionPath
			}
		}
	}

	sessionEntries, err := os.ReadDir(s.sessionsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return scan, fmt.Errorf("read %s: %w", s.sessionsDir(), err)
	}
	for _, entry := range sessionEntries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(s.sessionsDir(), entry.Name())
		item, found, err := retainedPathInfo(path, entry)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		if !found {
			continue
		}
		item.isSession = true
		scan.entries = append(scan.entries, item)
	}

	cacheEntries, err := os.ReadDir(s.CacheDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return scan, fmt.Errorf("read %s: %w", s.CacheDir, err)
	}
	for _, entry := range cacheEntries {
		path := filepath.Join(s.CacheDir, entry.Name())
		item, found, err := retainedPathInfo(path, entry)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		if !found {
			continue
		}
		if entry.Name() == ownershipMarker {
			scan.fixedBytes += item.bytes
			continue
		}
		scan.entries = append(scan.entries, item)
	}

	for _, root := range []string{s.DataDir, s.StateDir} {
		for _, name := range []string{ownershipMarker, "server.lock"} {
			path := filepath.Join(root, name)
			info, err := os.Stat(path)
			if err == nil {
				scan.fixedBytes += info.Size()
			} else if !errors.Is(err, os.ErrNotExist) {
				return scan, fmt.Errorf("stat %s: %w", path, err)
			}
		}
	}
	scan.skipped = errors.Join(skipped...)
	return scan, nil
}

func (s *Store) cleanupSessionIfUnreferencedLocked(sessionID string) error {
	if sessionID == "" {
		return nil
	}
	safe := safeName(sessionID)
	sessionPath := filepath.Join(s.sessionsDir(), safe)
	if s.pins[sessionPath] > 0 {
		s.orphanSessions[sessionID] = struct{}{}
		return nil
	}
	refs, err := s.liveSessionRefsLocked(safe)
	if err != nil {
		return err
	}
	if refs > 0 {
		delete(s.orphanSessions, sessionID)
		return nil
	}
	if err := os.RemoveAll(sessionPath); err != nil {
		return fmt.Errorf("remove %s: %w", sessionPath, err)
	}
	if err := os.RemoveAll(s.sessionLinksDir(safe)); err != nil {
		return fmt.Errorf("remove retention links for %s: %w", sessionPath, err)
	}
	delete(s.orphanSessions, sessionID)
	return nil
}

// liveSessionRefsLocked counts the retention links naming a session whose
// response record still exists, dropping any link left behind by a crash. It
// must be called with s.mu held: SaveResponse writes the link and the record
// inside one critical section, so only with the lock held does a link without a
// record definitively mean residue rather than a save in progress.
func (s *Store) liveSessionRefsLocked(sessionSafeName string) (int, error) {
	dir := s.sessionLinksDir(sessionSafeName)
	links, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", dir, err)
	}
	live := 0
	var errs []error
	for _, link := range links {
		record := filepath.Join(s.responsesDir(), link.Name()+".json")
		_, statErr := os.Stat(record)
		if statErr == nil {
			live++
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			// Retain the reference: an unreadable record is not proof that the
			// session is collectable.
			live++
			errs = append(errs, fmt.Errorf("stat %s: %w", record, statErr))
			continue
		}
		if rmErr := os.Remove(filepath.Join(dir, link.Name())); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			live++
			errs = append(errs, fmt.Errorf("remove stale retention link %s: %w", filepath.Join(dir, link.Name()), rmErr))
		}
	}
	return live, errors.Join(errs...)
}

func readResponseRecordPath(path string) (ResponseRecord, error) {
	var record ResponseRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return record, fmt.Errorf("read response record %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("decode response record %s: %w", path, err)
	}
	if err := migrateResponseRecord(&record); err != nil {
		return record, fmt.Errorf("response record %s: %w", path, err)
	}
	return record, nil
}

// retainedPathInfo measures one retained path. entry, when non-nil, is the
// os.DirEntry the enclosing ReadDir already produced. It reports whether the
// path still exists: the Copilot CLI subprocess writes and removes files under
// DataDir/sessions without taking s.mu, so paths routinely vanish mid-scan and
// that is not a retention failure.
func retainedPathInfo(path string, entry fs.DirEntry) (retainedEntry, bool, error) {
	item := retainedEntry{path: path}
	if entry != nil && !entry.IsDir() {
		// A plain file needs no walk; the enclosing ReadDir's entry already
		// carries (or can cheaply produce) everything age and size planning need.
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return item, false, nil
			}
			return item, false, fmt.Errorf("stat %s: %w", path, err)
		}
		item.modified = info.ModTime()
		item.bytes = info.Size()
		return item, true, nil
	}
	found := false
	err := filepath.WalkDir(path, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return fmt.Errorf("scan %s: %w", current, walkErr)
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if errors.Is(infoErr, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("stat %s: %w", current, infoErr)
		}
		found = true
		if info.ModTime().After(item.modified) {
			item.modified = info.ModTime()
		}
		if !d.IsDir() {
			item.bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return item, false, err
	}
	return item, found, nil
}
