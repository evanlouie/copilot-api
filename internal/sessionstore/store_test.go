package sessionstore

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestLockExcludesSecondProcess(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(store.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := AcquireLock(store.LockPath()); err == nil {
		t.Fatal("expected second lock acquisition to fail")
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
