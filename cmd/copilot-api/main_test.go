package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func TestRunCommandDispatch(t *testing.T) {
	if err := run([]string{"help"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run unknown error = %v", err)
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		if !isLoopbackListenAddr(addr) {
			t.Errorf("isLoopbackListenAddr(%q) = false", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "invalid"} {
		if isLoopbackListenAddr(addr) {
			t.Errorf("isLoopbackListenAddr(%q) = true", addr)
		}
	}
}

func TestRetentionLoopPrunesIdleExpiredState(t *testing.T) {
	root := t.TempDir()
	store := sessionstore.New(filepath.Join(root, "data"), filepath.Join(root, "state"), filepath.Join(root, "cache"))
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := sessionstore.ResponseRecord{ID: "resp_expired", Stored: true}
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "state", "responses", "resp_expired.json"), old, old); err != nil {
		t.Fatal(err)
	}
	store.SetRetentionPolicy(sessionstore.RetentionPolicy{MaxAge: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runRetentionLoop(ctx, store, slog.Default(), time.Millisecond)
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := store.LoadResponse(record.ID); errors.Is(err, sessionstore.ErrNotFound) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("idle expired response was not pruned")
}

func TestServeAcquiresLifecycleLockBeforeCreatingStorageRoots(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(root, "state")
	cacheDir := filepath.Join(root, "cache")
	configDir := filepath.Join(root, "config")
	for key, value := range map[string]string{"COPILOT_API_DATA_DIR": dataDir, "COPILOT_API_STATE_DIR": stateDir, "COPILOT_API_CACHE_DIR": cacheDir, "COPILOT_API_CONFIG_DIR": configDir} {
		t.Setenv(key, value)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := sessionstore.AcquireLock(sessionstore.LifecycleLockPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	if err := serve(nil); err == nil {
		t.Fatal("serve unexpectedly acquired held lifecycle lock")
	}
	for _, path := range []string{dataDir, stateDir, cacheDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("storage root %s was mutated before lifecycle lock: %v", path, err)
		}
	}
}

func TestPruneDryRunOnCleanInstallation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COPILOT_API_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("COPILOT_API_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("COPILOT_API_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("COPILOT_API_CONFIG_DIR", filepath.Join(root, "config"))
	if err := prune([]string{"--dry-run"}); err != nil {
		t.Fatal(err)
	}
}

func TestPruneDryRunAndRun(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(root, "state")
	cacheDir := filepath.Join(root, "cache")
	t.Setenv("COPILOT_API_DATA_DIR", dataDir)
	t.Setenv("COPILOT_API_STATE_DIR", stateDir)
	t.Setenv("COPILOT_API_CACHE_DIR", cacheDir)
	t.Setenv("COPILOT_API_CONFIG_DIR", filepath.Join(root, "config"))
	t.Setenv("COPILOT_RETENTION_MAX_RESPONSES", "1")
	store := sessionstore.New(dataDir, stateDir, cacheDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"resp_old", "resp_new"} {
		if err := store.SaveResponse(sessionstore.ResponseRecord{ID: id, Stored: true}); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(stateDir, "responses", "resp_old.json"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := prune([]string{"--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadResponse("resp_old"); err != nil {
		t.Fatalf("dry run removed response: %v", err)
	}
	if err := prune(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadResponse("resp_old"); !errors.Is(err, sessionstore.ErrNotFound) {
		t.Fatalf("old response remained: %v", err)
	}
}

func TestPruneDoesNotCreateMissingStateRoot(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(root, "state")
	cacheDir := filepath.Join(root, "cache")
	for key, value := range map[string]string{"COPILOT_API_DATA_DIR": dataDir, "COPILOT_API_STATE_DIR": stateDir, "COPILOT_API_CACHE_DIR": cacheDir, "COPILOT_API_CONFIG_DIR": filepath.Join(root, "config")} {
		t.Setenv(key, value)
	}
	store := sessionstore.New(dataDir, stateDir, cacheDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := prune(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prune created missing state root: %v", err)
	}
}

func TestPurgeDoesNotCreateMissingStateRoot(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(root, "state")
	cacheDir := filepath.Join(root, "cache")
	for key, value := range map[string]string{"COPILOT_API_DATA_DIR": dataDir, "COPILOT_API_STATE_DIR": stateDir, "COPILOT_API_CACHE_DIR": cacheDir, "COPILOT_API_CONFIG_DIR": filepath.Join(root, "config")} {
		t.Setenv(key, value)
	}
	store := sessionstore.New(dataDir, stateDir, cacheDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := purge([]string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dataDir, stateDir, cacheDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("purge left or created %s: %v", path, err)
		}
	}
}

func TestPurgeDryRunAndConfirmed(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	t.Setenv("COPILOT_API_DATA_DIR", dataDir)
	t.Setenv("COPILOT_API_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("COPILOT_API_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("COPILOT_API_CONFIG_DIR", filepath.Join(root, "config"))
	store := sessionstore.New(dataDir, filepath.Join(root, "state"), filepath.Join(root, "cache"))
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(dataDir, "retained")
	if err := os.WriteFile(retained, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := purge([]string{"--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("dry-run removed retained data: %v", err)
	}
	if err := purge([]string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("purge left data directory: %v", err)
	}
}
