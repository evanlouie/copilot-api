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

func TestProviderHardensExistingFileAndDirectoryModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session")
	dir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(file, []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o666); err != nil {
		t.Fatal(err)
	}
	provider := &Provider{root: root}
	if err := provider.WriteFile("/session-state/events.jsonl", "new", nil); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes = dir %o file %o", dirInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func TestProviderRejectsSymlinkComponents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session")
	outside := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	provider := &Provider{root: root}
	if err := provider.WriteFile("/linked/escape", "secret", nil); err == nil {
		t.Fatal("write through symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape")); !os.IsNotExist(err) {
		t.Fatalf("outside file created: %v", err)
	}
}

func TestSafeSessionIDIsInjectiveForUnsafeValues(t *testing.T) {
	if safeSessionID("a/b") == safeSessionID("a?b") {
		t.Fatal("unsafe session IDs collided")
	}
	if safeSessionID(".") == "." || safeSessionID("..") == ".." {
		t.Fatal("dot segment was preserved")
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

func TestWriteEventsRejectsSymlinkedSessionTree(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "sessions")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := WriteEvents(root, "abc", []byte("{}\n")); err == nil {
		t.Fatal("WriteEvents followed symlinked sessions directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "abc")); !os.IsNotExist(err) {
		t.Fatalf("outside session created: %v", err)
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
