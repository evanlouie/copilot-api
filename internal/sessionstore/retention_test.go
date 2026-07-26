package sessionstore

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// seedReferencedSession writes one live response and the SDK session directory
// it references, backdating the session past any MaxAge a caller will set.
func seedReferencedSession(t *testing.T, store *Store, sessionID, responseID string, age time.Duration) string {
	t.Helper()
	sessionPath := filepath.Join(store.sessionsDir(), safeName(sessionID))
	if err := os.MkdirAll(sessionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResponse(ResponseRecord{ID: responseID, SDKSessionID: sessionID, Stored: true}); err != nil {
		t.Fatal(err)
	}
	backdate(t, sessionPath, age)
	return sessionPath
}

// TestPruneKeepsSessionReferencedByLiveResponse pins down the safety property
// the retention link index exists to provide: a session a stored response can
// still be resumed from is off limits to every quota, whether or not the index
// happens to be on disk when the prune starts. Losing the session directory
// while its record survives makes that conversation permanently unresumable, so
// "no index" must mean "delete no session", never "nothing is referenced".
func TestPruneKeepsSessionReferencedByLiveResponse(t *testing.T) {
	for _, tc := range []struct {
		name        string
		removeIndex bool
	}{
		{name: "index present"},
		{name: "index missing", removeIndex: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := New(t.TempDir(), t.TempDir(), t.TempDir())
			if err := store.Ensure(); err != nil {
				t.Fatal(err)
			}
			const sessionID, responseID = "sess_live", "resp_live"
			sessionPath := seedReferencedSession(t, store, sessionID, responseID, 48*time.Hour)
			if tc.removeIndex {
				if err := os.RemoveAll(store.linksDir()); err != nil {
					t.Fatal(err)
				}
			}
			store.SetRetentionPolicy(RetentionPolicy{MaxAge: 24 * time.Hour})
			report, err := store.Prune(false)
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if slices.Contains(report.Paths, sessionPath) {
				t.Fatalf("prune reported deleting a referenced session: %#v", report.Paths)
			}
			if _, err := os.Stat(sessionPath); err != nil {
				t.Fatalf("session still referenced by %s was deleted: %v", responseID, err)
			}
			if _, err := store.LoadResponseForContinuation(responseID); err != nil {
				t.Fatalf("response record disappeared: %v", err)
			}
			link := filepath.Join(store.sessionLinksDir(safeName(sessionID)), safeName(responseID))
			if _, err := os.Stat(link); err != nil {
				t.Fatalf("prune did not (re)build the retention link index: %v", err)
			}
		})
	}
}

// TestPruneKeepsEverySessionWhenIndexUnreadable covers the index being present
// but unreadable, which no rebuild can repair. Response quotas still apply;
// sessions are all treated as referenced.
func TestPruneKeepsEverySessionWhenIndexUnreadable(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	referenced := seedReferencedSession(t, store, "sess_live", "resp_live", 48*time.Hour)
	orphan := filepath.Join(store.sessionsDir(), "sess_orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	backdate(t, orphan, 48*time.Hour)
	if err := store.SaveResponse(ResponseRecord{ID: "resp_expired", Stored: true}); err != nil {
		t.Fatal(err)
	}
	backdate(t, store.responsePath("resp_expired"), 48*time.Hour)

	if err := os.Chmod(store.linksDir(), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.linksDir(), 0o700) })
	if _, err := os.ReadDir(store.linksDir()); err == nil {
		t.Skip("filesystem does not enforce directory read permission")
	}

	store.SetRetentionPolicy(RetentionPolicy{MaxAge: 24 * time.Hour})
	report, err := store.Prune(false)
	if err == nil {
		t.Fatal("expected the unreadable retention index to be reported to the caller")
	}
	if !strings.Contains(err.Error(), retentionLinkDir) {
		t.Fatalf("prune error did not name the retention index: %v", err)
	}
	for _, path := range []string{referenced, orphan} {
		if slices.Contains(report.Paths, path) {
			t.Fatalf("prune reported deleting a session with no readable index: %#v", report.Paths)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("session %s was deleted with no readable index: %v", path, err)
		}
	}
	// Sessions are protected, but the response quota still runs.
	if _, err := os.Stat(store.responsePath("resp_expired")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired response was not pruned: %v", err)
	}
	if err := store.TakeMaintenanceError(); err != nil {
		t.Fatalf("a retryable index failure latched readiness off: %v", err)
	}
}

// TestPruneKeepsSessionsWhenIndexCannotBeBuilt covers the third way the index
// can be unavailable: it is absent and the rebuild itself fails.
func TestPruneKeepsSessionsWhenIndexCannotBeBuilt(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	sessionPath := seedReferencedSession(t, store, "sess_live", "resp_live", 48*time.Hour)
	if err := os.RemoveAll(store.linksDir()); err != nil {
		t.Fatal(err)
	}
	// A read-only responses directory lets the scan run but makes staging the
	// rebuilt index impossible.
	if err := os.Chmod(store.responsesDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.responsesDir(), 0o700) })
	probe := filepath.Join(store.responsesDir(), ".probe")
	if err := os.Mkdir(probe, 0o700); err == nil {
		_ = os.Remove(probe)
		t.Skip("filesystem does not enforce directory write permission")
	}

	store.SetRetentionPolicy(RetentionPolicy{MaxAge: 24 * time.Hour})
	report, err := store.Prune(false)
	if err == nil {
		t.Fatal("expected the failed index build to be reported to the caller")
	}
	if slices.Contains(report.Paths, sessionPath) {
		t.Fatalf("prune reported deleting a session with no index: %#v", report.Paths)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("session was deleted after the index build failed: %v", err)
	}
	if err := store.TakeMaintenanceError(); err != nil {
		t.Fatalf("a retryable index failure latched readiness off: %v", err)
	}
}

// TestDeleteResponseKeepsSessionWhenIndexRemoved holds the delete path to the
// same rule as the prune path: an index that cannot be established says nothing
// about which sessions are still referenced, so no session may be deleted.
// Deleting one here would leave resp_live pointing at a session directory that
// no longer exists, making that conversation permanently unresumable.
func TestDeleteResponseKeepsSessionWhenIndexRemoved(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	sessionPath := seedReferencedSession(t, store, "sdk_shared_index", "resp_doomed", 0)
	if err := store.SaveResponse(ResponseRecord{ID: "resp_live", SDKSessionID: "sdk_shared_index", Stored: true}); err != nil {
		t.Fatal(err)
	}
	// The index disappears out from under a running server.
	if err := os.RemoveAll(store.linksDir()); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteResponse("resp_doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("session still referenced by resp_live was removed: %v", err)
	}
	if _, err := store.LoadResponse("resp_live"); err != nil {
		t.Fatalf("load surviving response: %v", err)
	}
	if err := store.TakeMaintenanceError(); err != nil {
		t.Fatalf("a recoverable index rebuild latched readiness off: %v", err)
	}
	// The rebuilt index must leave cleanup working, not disabled forever.
	if err := store.DeleteResponse("resp_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced session remained after the index was rebuilt: %v", err)
	}
}

// TestDeleteResponseKeepsSessionWhenLinkDirUnreadable covers the third case: the
// index root exists, so a missing per-session directory would mean zero
// references, but this one exists and cannot be listed. 0o300 keeps the
// directory writable so removing the deleted response's own link still
// succeeds, isolating the unreadable-listing case.
func TestDeleteResponseKeepsSessionWhenLinkDirUnreadable(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	sessionPath := seedReferencedSession(t, store, "sdk_opaque_links", "resp_opaque", 0)
	linkDir := store.sessionLinksDir(safeName("sdk_opaque_links"))
	if err := os.Chmod(linkDir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(linkDir, 0o700) })
	if _, err := os.ReadDir(linkDir); err == nil {
		t.Skip("filesystem does not enforce directory read permission")
	}
	if err := store.DeleteResponse("resp_opaque"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("session was deleted from an unreadable link directory: %v", err)
	}
}

// TestPruneWithoutEnsureMatchesEnsuredStore exercises the prune subcommand,
// which builds a Store and calls Prune without ever calling Ensure. Prune has
// to establish the index itself, otherwise it plans session deletions from an
// index that was never built.
func TestPruneWithoutEnsureMatchesEnsuredStore(t *testing.T) {
	run := func(t *testing.T, ensure bool) []string {
		t.Helper()
		dataDir, stateDir, cacheDir := t.TempDir(), t.TempDir(), t.TempDir()
		seed := New(dataDir, stateDir, cacheDir)
		if err := seed.Ensure(); err != nil {
			t.Fatal(err)
		}
		live := seedReferencedSession(t, seed, "sess_live", "resp_live", 48*time.Hour)
		orphan := filepath.Join(seed.sessionsDir(), "sess_orphan")
		if err := os.MkdirAll(orphan, 0o700); err != nil {
			t.Fatal(err)
		}
		backdate(t, orphan, 48*time.Hour)
		// A store written before the retention index existed.
		if err := os.RemoveAll(seed.linksDir()); err != nil {
			t.Fatal(err)
		}

		store := New(dataDir, stateDir, cacheDir)
		if ensure {
			if err := store.Ensure(); err != nil {
				t.Fatal(err)
			}
		}
		store.SetRetentionPolicy(RetentionPolicy{MaxAge: 24 * time.Hour})
		report, err := store.Prune(false)
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
		if _, err := os.Stat(store.linksDir()); err != nil {
			t.Fatalf("prune left the retention index unbuilt: %v", err)
		}
		if _, err := os.Stat(live); err != nil {
			t.Fatalf("referenced session was deleted: %v", err)
		}
		if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired unreferenced session remained: %v", err)
		}
		relative := make([]string, 0, len(report.Paths))
		for _, path := range report.Paths {
			rel, err := filepath.Rel(dataDir, path)
			if err != nil {
				if rel, err = filepath.Rel(stateDir, path); err != nil {
					t.Fatal(err)
				}
			}
			relative = append(relative, rel)
		}
		sort.Strings(relative)
		return relative
	}

	var ensured, unensured []string
	t.Run("with Ensure", func(t *testing.T) { ensured = run(t, true) })
	t.Run("without Ensure", func(t *testing.T) { unensured = run(t, false) })
	if !slices.Equal(ensured, unensured) {
		t.Fatalf("prune without Ensure diverged: %#v != %#v", unensured, ensured)
	}
}

// TestMaxAgePruneCascadesToSessionOrphanedByResponse guards the cascade the
// index enables: once the quota deletes the last response referencing a
// session, that session becomes collectable in the same pass.
func TestMaxAgePruneCascadesToSessionOrphanedByResponse(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	const sessionID, responseID = "sess_stale", "resp_stale"
	sessionPath := seedReferencedSession(t, store, sessionID, responseID, 48*time.Hour)
	backdate(t, store.responsePath(responseID), 48*time.Hour)

	store.SetRetentionPolicy(RetentionPolicy{MaxAge: 24 * time.Hour})
	report, err := store.Prune(false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, path := range []string{store.responsePath(responseID), sessionPath} {
		if !slices.Contains(report.Paths, path) {
			t.Fatalf("prune report omitted %s: %#v", path, report.Paths)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remained: %v", path, err)
		}
	}
	if _, err := os.Stat(store.sessionLinksDir(safeName(sessionID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retention links outlived the session they described: %v", err)
	}
}
