package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestPurgeDryRunAndConfirmed(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	t.Setenv("COPILOT_API_DATA_DIR", dataDir)
	t.Setenv("COPILOT_API_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("COPILOT_API_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("COPILOT_API_CONFIG_DIR", filepath.Join(root, "config"))
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
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
