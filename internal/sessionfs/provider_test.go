package sessionfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProviderPreventsTraversal(t *testing.T) {
	root := t.TempDir()
	p := &Provider{root: filepath.Join(root, "session")}
	if err := p.WriteFile("/session-state/../events.jsonl", "ok", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "events.jsonl")); err == nil {
		t.Fatal("write escaped session root")
	}
	got, err := p.ReadFile("/session-state/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ok" {
		t.Fatalf("got %q", got)
	}
}

func TestEnsureSessionCreatesReadableProviderRoot(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	sessionID := "resp/sdk:1"
	if err := m.EnsureSession(sessionID); err != nil {
		t.Fatal(err)
	}
	p := m.Provider(sessionID)
	info, err := p.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDirectory || info.IsFile {
		t.Fatalf("root info = %#v, want directory", info)
	}
	entries, err := p.ReadDirectory("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("root entries = %#v, want empty", entries)
	}
	typedEntries, err := p.ReadDirectoryWithTypes("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(typedEntries) != 0 {
		t.Fatalf("typed root entries = %#v, want empty", typedEntries)
	}
}

func TestManagerSharesLockForSessionWithoutRetainingProviders(t *testing.T) {
	manager := NewManager(t.TempDir())
	first := manager.Provider("session-1")
	second := manager.Provider("session-1")
	if first == second {
		t.Fatal("Provider unexpectedly retained a provider instance")
	}
	if first.mutex() != second.mutex() {
		t.Fatal("Provider returned independent locks for the same session root")
	}
}

func TestWriteEvents(t *testing.T) {
	root := t.TempDir()
	path, err := WriteEvents(root, "abc", []byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
