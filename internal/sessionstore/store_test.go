package sessionstore

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSafeNameIsInjectiveAndRejectsDotSegments(t *testing.T) {
	if safeName("a/b") == safeName("a?b") {
		t.Fatal("unsafe response IDs collided")
	}
	if safeName(".") == "." || safeName("..") == ".." {
		t.Fatal("dot segment was preserved")
	}
	if strings.EqualFold(safeName("resp_id"), safeName("RESP_ID")) || strings.EqualFold(safeName("resp_id"), safeName("resp_id.")) {
		t.Fatal("Windows-equivalent response IDs share a filename")
	}
	for _, reserved := range []string{"con", "NUL.txt", "com1", "LPT9.log"} {
		if safeName(reserved) == reserved {
			t.Fatalf("Windows reserved name %q was preserved", reserved)
		}
	}
}

func TestLockExcludesSecondProcess(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(store.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	if _, err := AcquireLock(store.LockPath()); err == nil {
		t.Fatal("expected second lock acquisition to fail")
	}
}

func TestAtomicWriteCleansTemporaryFileAfterRenameFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(target, map[string]string{"value": "x"}); err == nil {
		t.Fatal("expected rename over directory to fail")
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".copilot-api-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files leaked: %v", matches)
	}
}

func TestSaveResponseWritesVersionedCompactJSON(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResponse(ResponseRecord{ID: "resp_compact", SDKSessionID: "sdk", Model: "gpt-5", Stored: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(store.responsePath("resp_compact"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version":3`) {
		t.Fatalf("record JSON missing version: %s", b)
	}
	if strings.Contains(string(b), "\n  ") {
		t.Fatalf("record JSON should be compact, got: %s", b)
	}
}

func TestEnsureMigratesLegacyOwnershipMarkers(t *testing.T) {
	root := t.TempDir()
	store := New(filepath.Join(root, "data"), filepath.Join(root, "state"), filepath.Join(root, "cache"))
	for _, dir := range []string{store.DataDir, store.StateDir, store.CacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ownershipMarker), []byte(legacyOwnershipMarkerContent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Ensure(); err != nil {
		t.Fatalf("legacy store was rejected: %v", err)
	}
	for _, dir := range []string{store.DataDir, store.StateDir, store.CacheDir} {
		content, err := os.ReadFile(filepath.Join(dir, ownershipMarker))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != ownershipMarkerContent {
			t.Fatalf("marker in %s was not migrated: %q", dir, content)
		}
	}
}

func TestEnsureRefusesToClaimNonEmptyRoot(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "unrelated.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(data, filepath.Join(root, "state"), filepath.Join(root, "cache"))
	if err := store.Ensure(); err == nil || !strings.Contains(err.Error(), "refusing to claim") {
		t.Fatalf("Ensure error = %v, want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(data, ownershipMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe root was marked: %v", err)
	}
}

func TestValidateRootsRejectsAncestorOfWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := New(filepath.Dir(cwd), filepath.Join(root, "state"), filepath.Join(root, "cache"))
	if _, err := store.ValidateRoots(); err == nil {
		t.Fatal("expected ancestor of working directory to be rejected")
	}
}

func TestLoadResponseMigratesOldVersionsAndRejectsFuture(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	oldPath := store.responsePath("resp_old")
	if err := os.WriteFile(oldPath, []byte(`{"version":2,"id":"resp_old","stored":true,"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":3},"total_tokens":17}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := store.LoadResponse("resp_old")
	if err != nil {
		t.Fatal(err)
	}
	if old.Version != ResponseRecordVersion || old.Status != "completed" || old.Usage == nil || old.Usage.InputTokensDetails == nil || old.Usage.InputTokensDetails.CachedTokens == nil || *old.Usage.InputTokensDetails.CachedTokens != 4 || old.Usage.OutputTokensDetails == nil || old.Usage.OutputTokensDetails.ReasoningTokens == nil || *old.Usage.OutputTokensDetails.ReasoningTokens != 3 {
		t.Fatalf("migrated record = %#v", old)
	}
	completionOnlyPath := store.responsePath("resp_v1_completion")
	if err := os.WriteFile(completionOnlyPath, []byte(`{"version":1,"id":"resp_v1_completion","stored":true,"usage":{"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2},"total_tokens":5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	completionOnly, err := store.LoadResponse("resp_v1_completion")
	if err != nil {
		t.Fatal(err)
	}
	if completionOnly.Usage == nil || completionOnly.Usage.OutputTokens == nil || *completionOnly.Usage.OutputTokens != 5 || completionOnly.Usage.OutputTokensDetails == nil || completionOnly.Usage.OutputTokensDetails.ReasoningTokens == nil || *completionOnly.Usage.OutputTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("completion-only legacy usage was not migrated: %#v", completionOnly.Usage)
	}

	missingVersionPath := store.responsePath("resp_missing_version")
	if err := os.WriteFile(missingVersionPath, []byte(`{"id":"resp_missing_version","stored":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unversioned, err := store.LoadResponse("resp_missing_version")
	if err != nil {
		t.Fatalf("valid pre-versioning record was rejected: %v", err)
	}
	if unversioned.Version != ResponseRecordVersion || unversioned.Status != "completed" {
		t.Fatalf("unversioned record was not migrated: %#v", unversioned)
	}
	malformedPath := store.responsePath("resp_malformed")
	if err := os.WriteFile(malformedPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadResponse("resp_malformed"); err == nil {
		t.Fatal("malformed unversioned record was accepted")
	}
	futurePath := store.responsePath("resp_future")
	if err := os.WriteFile(futurePath, []byte(`{"version":999,"id":"resp_future","stored":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.LoadResponse("resp_future")
	var versionErr *UnsupportedRecordVersionError
	if !errors.As(err, &versionErr) {
		t.Fatalf("future version error = %v", err)
	}
}

func TestDeleteResponseWritesMinimalTombstone(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := ResponseRecord{ID: "resp_delete", Stored: true, InputText: strings.Repeat("secret", 100), OutputText: strings.Repeat("answer", 100)}
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteResponse(record.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := store.loadResponseRecord(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Deleted || stored.InputText != "" || stored.OutputText != "" || len(stored.Output) != 0 {
		t.Fatalf("tombstone retained content: %#v", stored)
	}
	if err := store.SaveResponse(record); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late save error = %v, want ErrNotFound", err)
	}
}

func TestConcurrentSaveCannotResurrectDeletedResponse(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := ResponseRecord{ID: "resp_race", Stored: true, OutputText: "answer"}
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	go func() {
		<-start
		for range 100 {
			_ = store.SaveResponse(record)
		}
		done <- struct{}{}
	}()
	go func() {
		<-start
		_ = store.DeleteResponse(record.ID)
		done <- struct{}{}
	}()
	close(start)
	<-done
	<-done
	stored, err := store.loadResponseRecord(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Deleted {
		t.Fatalf("response was resurrected: %#v", stored)
	}
}

func TestPinnedTombstoneBlocksLateSaveUntilRunnerReleases(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := ResponseRecord{ID: "resp_deleted", Stored: true, OutputText: "answer"}
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	release := store.PinResponse(record.ID)
	if err := store.DeleteResponse(record.ID); err != nil {
		t.Fatal(err)
	}
	store.SetRetentionPolicy(RetentionPolicy{MaxBytes: 1})
	if _, err := store.Prune(false); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResponse(record); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late save error = %v, want ErrNotFound", err)
	}
	release()
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted ID guard leaked after active runner release: %#v", store.deletedIDs)
	}
	if _, err := store.Prune(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.responsePath(record.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released tombstone was not pruned: %v", err)
	}
}

func TestPruneSkipsPinnedResponseAndSession(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	sessionID := "sdk_pinned"
	sessionPath := filepath.Join(store.sessionsDir(), safeName(sessionID))
	if err := os.MkdirAll(sessionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	record := ResponseRecord{ID: "resp_pinned", SDKSessionID: sessionID, Stored: true}
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(store.responsePath(record.ID), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sessionPath, old, old); err != nil {
		t.Fatal(err)
	}
	releaseResponse := store.PinResponse(record.ID)
	releaseSession := store.PinSession(sessionID)
	store.SetRetentionPolicy(RetentionPolicy{MaxAge: time.Second})
	if _, err := store.Prune(false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadResponse(record.ID); err != nil {
		t.Fatalf("pinned response was pruned: %v", err)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("pinned session was pruned: %v", err)
	}
	releaseResponse()
	releaseSession()
}

func TestDeleteResponseCleansSessionAfterFinalReference(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	sessionID := "sdk_shared"
	sessionPath := filepath.Join(store.sessionsDir(), safeName(sessionID))
	if err := os.MkdirAll(sessionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"resp_one", "resp_two"} {
		if err := store.SaveResponse(ResponseRecord{ID: id, SDKSessionID: sessionID, Stored: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeleteResponse("resp_one"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("shared session removed too early: %v", err)
	}
	if err := store.DeleteResponse("resp_two"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced session remained: %v", err)
	}
}

func TestPinnedSessionIsCleanedWhenReleasedAfterDelete(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	sessionID := "sdk_active"
	sessionPath := filepath.Join(store.sessionsDir(), safeName(sessionID))
	if err := os.MkdirAll(sessionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResponse(ResponseRecord{ID: "resp_active", SDKSessionID: sessionID, Stored: true}); err != nil {
		t.Fatal(err)
	}
	release := store.PinSession(sessionID)
	if err := store.DeleteResponse("resp_active"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("pinned session removed: %v", err)
	}
	release()
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan session remained after unpin: %v", err)
	}
}

func TestResponseCountPruneCleansUnreferencedSession(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct{ responseID, sessionID string }{{"resp_old", "sdk_old"}, {"resp_new", "sdk_new"}} {
		if err := os.MkdirAll(filepath.Join(store.sessionsDir(), safeName(value.sessionID)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveResponse(ResponseRecord{ID: value.responseID, SDKSessionID: value.sessionID, Stored: true}); err != nil {
			t.Fatal(err)
		}
		if value.responseID == "resp_old" {
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(store.responsePath(value.responseID), old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filepath.Join(store.sessionsDir(), safeName(value.sessionID)), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	store.SetRetentionPolicy(RetentionPolicy{MaxResponses: 1})
	dryRun, err := store.Prune(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dryRun.Paths) != 2 || !slices.Contains(dryRun.Paths, store.responsePath("resp_old")) || !slices.Contains(dryRun.Paths, filepath.Join(store.sessionsDir(), "sdk_old")) {
		t.Fatalf("dry-run did not report exact response/session deletion set: %#v", dryRun.Paths)
	}
	if _, err := store.Prune(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.sessionsDir(), "sdk_old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old session remained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.sessionsDir(), "sdk_new")); err != nil {
		t.Fatalf("new session was removed: %v", err)
	}
}

func TestBytePruneCreditsSessionOrphanedBySelectedResponse(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct{ responseID, sessionID string }{{"resp_old_bytes", "sdk_old_bytes"}, {"resp_new_bytes", "sdk_new_bytes"}} {
		sessionPath := filepath.Join(store.sessionsDir(), safeName(value.sessionID))
		if err := os.MkdirAll(sessionPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sessionPath, "payload"), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveResponse(ResponseRecord{ID: value.responseID, SDKSessionID: value.sessionID, Stored: true}); err != nil {
			t.Fatal(err)
		}
		if value.responseID == "resp_old_bytes" {
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(store.responsePath(value.responseID), old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(sessionPath, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, fixed, err := store.retentionEntries()
	if err != nil {
		t.Fatal(err)
	}
	var total, oldReclaim int64 = fixed, 0
	for _, entry := range entries {
		total += entry.bytes
		if strings.Contains(entry.path, "resp_old_bytes") || strings.Contains(entry.path, "sdk_old_bytes") {
			oldReclaim += entry.bytes
		}
	}
	store.SetRetentionPolicy(RetentionPolicy{MaxBytes: total - oldReclaim})
	if _, err := store.Prune(false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadResponse("resp_old_bytes"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old response remained: %v", err)
	}
	if _, err := store.LoadResponse("resp_new_bytes"); err != nil {
		t.Fatalf("new response was over-pruned: %v", err)
	}
}

func TestResponseCountIncludesTombstones(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"resp_old_deleted", "resp_new_deleted"} {
		if err := store.SaveResponse(ResponseRecord{ID: id, Stored: true}); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteResponse(id); err != nil {
			t.Fatal(err)
		}
		if id == "resp_old_deleted" {
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(store.responsePath(id), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	store.SetRetentionPolicy(RetentionPolicy{MaxResponses: 1})
	if _, err := store.Prune(false); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store.responsesDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != safeName("resp_new_deleted")+".json" {
		t.Fatalf("retained tombstones = %#v", entries)
	}
}

func TestPruneHonorsResponseCountAndDryRun(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	store.SetRetentionPolicy(RetentionPolicy{MaxResponses: 1})
	for _, id := range []string{"resp_old", "resp_new"} {
		if err := store.SaveResponse(ResponseRecord{ID: id, Stored: true}); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(store.responsePath("resp_old"), old, old); err != nil {
		t.Fatal(err)
	}
	report, err := store.Prune(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Paths) != 1 || !strings.Contains(report.Paths[0], "resp_old") {
		t.Fatalf("dry-run report = %#v", report)
	}
	if _, err := store.LoadResponse("resp_old"); err != nil {
		t.Fatalf("dry run removed response: %v", err)
	}
	if _, err := store.Prune(false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadResponse("resp_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old response remained: %v", err)
	}
	if _, err := store.LoadResponse("resp_new"); err != nil {
		t.Fatalf("new response pruned: %v", err)
	}
}

func TestResponseStoreVisibility(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	rec := ResponseRecord{ID: "resp_1", SDKSessionID: "sdk", Model: "gpt-5", Stored: false}
	if err := store.SaveResponse(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadResponse("resp_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("store:false response should not be API-visible, got %v", err)
	}
	if _, err := store.LoadResponseForContinuation("resp_1"); err != nil {
		t.Fatalf("store:false response should remain available for continuation/debug metadata: %v", err)
	}
}
