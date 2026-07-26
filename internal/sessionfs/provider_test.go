package sessionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestProvider(t *testing.T, sessionID string) (*Manager, *Provider) {
	t.Helper()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.EnsureSession(sessionID); err != nil {
		t.Fatal(err)
	}
	return manager, manager.Provider(sessionID)
}

// symlinksAvailable reports whether the platform lets the test plant symlinks.
func symlinksAvailable(t *testing.T, target string) bool {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(t.TempDir(), "probe")); err != nil {
		t.Logf("symlinks unavailable: %v", err)
		return false
	}
	return true
}

func TestProviderRejectsTraversal(t *testing.T) {
	t.Parallel()
	manager, p := newTestProvider(t, "session")
	// ".." used to be silently dropped, which quietly redirected the request to
	// a different file. It is now rejected outright.
	if err := p.WriteFile("/session-state/../events.jsonl", "ok", nil); err == nil {
		t.Fatal("traversal write was accepted")
	}
	if _, err := os.Stat(filepath.Join(manager.SessionRoot("session"), "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("traversal write created a file: %v", err)
	}
	if _, err := p.ReadFile("../../etc/passwd"); err == nil {
		t.Fatal("traversal read was accepted")
	}
	if err := p.AppendFile("/../escape", "ok", nil); err == nil {
		t.Fatal("traversal append was accepted")
	}
	if err := p.MakeDirectory("/a/../../b", true, nil); err == nil {
		t.Fatal("traversal mkdir was accepted")
	}
	if _, err := p.ReadDirectory("/.."); err == nil {
		t.Fatal("traversal readdir was accepted")
	}
	if _, err := p.Stat("/../session"); err == nil {
		t.Fatal("traversal stat was accepted")
	}
	if _, err := p.Exists("/../session"); err == nil {
		t.Fatal("traversal exists was accepted")
	}
	if err := p.Remove("/../session", true, true); err == nil {
		t.Fatal("traversal remove was accepted")
	}
	if err := p.Rename("/../session", "/moved"); err == nil {
		t.Fatal("traversal rename source was accepted")
	}
	if err := p.WriteFile("/session-state/events.jsonl", "ok", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Rename("/session-state/events.jsonl", "/../events.jsonl"); err == nil {
		t.Fatal("traversal rename destination was accepted")
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
	t.Parallel()
	manager, provider := newTestProvider(t, "session")
	root := manager.SessionRoot("session")
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
	t.Parallel()
	outside := t.TempDir()
	if !symlinksAvailable(t, outside) {
		t.Skip("symlinks unavailable")
	}
	manager, provider := newTestProvider(t, "session")
	if err := os.Symlink(outside, filepath.Join(manager.SessionRoot("session"), "linked")); err != nil {
		t.Fatal(err)
	}
	if err := provider.WriteFile("/linked/escape", "secret", nil); err == nil {
		t.Fatal("write through symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape")); !os.IsNotExist(err) {
		t.Fatalf("outside file created: %v", err)
	}
}

func TestProviderRejectsOperationsThroughSymlinkedDirectory(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	if !symlinksAvailable(t, outside) {
		t.Skip("symlinks unavailable")
	}
	manager, p := newTestProvider(t, "session")
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(manager.SessionRoot("session"), "linked")); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteFile("/plain", "seed", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadFile("/linked/secret"); err == nil {
		t.Fatal("read through symlink was accepted")
	}
	if err := p.WriteFile("/linked/secret", "owned", nil); err == nil {
		t.Fatal("write through symlink was accepted")
	}
	if err := p.AppendFile("/linked/secret", "owned", nil); err == nil {
		t.Fatal("append through symlink was accepted")
	}
	if err := p.Rename("/plain", "/linked/secret"); err == nil {
		t.Fatal("rename through symlink was accepted")
	}
	if err := p.Remove("/linked/secret", false, false); err == nil {
		t.Fatal("remove through symlink was accepted")
	}
	if err := p.MakeDirectory("/linked/sub", true, nil); err == nil {
		t.Fatal("mkdir through symlink was accepted")
	}
	if _, err := p.Stat("/linked/secret"); err == nil {
		t.Fatal("stat through symlink was accepted")
	}
	if _, err := p.ReadDirectory("/linked"); err == nil {
		t.Fatal("readdir through symlink was accepted")
	}
	if _, err := p.ReadDirectoryWithTypes("/linked"); err == nil {
		t.Fatal("typed readdir through symlink was accepted")
	}
	if _, err := p.Exists("/linked/secret"); err == nil {
		t.Fatal("exists through symlink was accepted")
	}
	content, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "original" {
		t.Fatalf("outside file was modified: %q", content)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("outside directory gained entries: %#v", entries)
	}
}

// A symlink at the final component cannot be written through either. Unlike a
// symlinked parent, replacing or unlinking the link itself stays inside the
// session, so those operations succeed on the link rather than failing.
func TestProviderDoesNotFollowSymlinkedFinalComponent(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	if !symlinksAvailable(t, outside) {
		t.Skip("symlinks unavailable")
	}
	manager, p := newTestProvider(t, "session")
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(manager.SessionRoot("session"), "leak")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadFile("/leak"); err == nil {
		t.Fatal("read followed the symlink out of the session")
	}
	if err := p.AppendFile("/leak", "owned", nil); err == nil {
		t.Fatal("append followed the symlink out of the session")
	}
	if err := p.WriteFile("/leak", "owned", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("write left the symlink in place instead of replacing it")
	}
	content, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "original" {
		t.Fatalf("outside file was modified: %q", content)
	}
}

// The pre-fix provider validated the path and then reopened it by name, so a
// symlink planted in that window redirected the write outside the session.
func TestProviderRejectsSymlinkPlantedDuringWrite(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	if !symlinksAvailable(t, outside) {
		t.Skip("symlinks unavailable")
	}
	manager, p := newTestProvider(t, "session")
	target := filepath.Join(outside, "escape")
	dir := filepath.Join(manager.SessionRoot("session"), "session-state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "events.jsonl")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(link)
			_ = os.Symlink(target, link)
		}
	}()
	for range soakIterations(150) {
		_ = p.WriteFile("/session-state/events.jsonl", "secret", nil)
	}
	close(stop)
	wg.Wait()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("write escaped through a symlink planted mid-write: %v", err)
	}
}

func TestProviderWriteFileIsAtomicForConcurrentReaders(t *testing.T) {
	t.Parallel()
	manager, p := newTestProvider(t, "session")
	const path = "/session-state/events.jsonl"
	first := strings.Repeat("a", 1<<16)
	second := strings.Repeat("b", 1<<14)
	if err := p.WriteFile(path, first, nil); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(manager.SessionRoot("session"), "session-state", "events.jsonl")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	check := func(source, content string) {
		if content != first && content != second {
			t.Errorf("%s observed a partially written file of %d bytes", source, len(content))
		}
	}
	// The raw reader bypasses the provider lock, so the assertion covers the
	// file replacement itself rather than only the mutex.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				content, err := os.ReadFile(full)
				if err != nil {
					t.Errorf("raw read failed: %v", err)
					return
				}
				check("raw reader", string(content))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			content, err := p.ReadFile(path)
			if err != nil {
				t.Errorf("provider read failed: %v", err)
				return
			}
			check("provider reader", content)
		}
	}()
	for i := range soakIterations(40) {
		content := first
		if i%2 == 0 {
			content = second
		}
		if err := p.WriteFile(path, content, nil); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	wg.Wait()
}

func TestProviderKeepsFilesUsableForRestrictiveModes(t *testing.T) {
	t.Parallel()
	_, p := newTestProvider(t, "session")
	for _, mode := range []int{0o444, 0o200, 0o000, 0o777} {
		path := fmt.Sprintf("/session-state/mode-%o", mode)
		if err := p.WriteFile(path, "one", &mode); err != nil {
			t.Fatalf("mode %o: %v", mode, err)
		}
		if err := p.AppendFile(path, "two", &mode); err != nil {
			t.Fatalf("mode %o: append after write failed: %v", mode, err)
		}
		if err := p.WriteFile(path, "three", &mode); err != nil {
			t.Fatalf("mode %o: rewrite failed: %v", mode, err)
		}
		got, err := p.ReadFile(path)
		if err != nil {
			t.Fatalf("mode %o: read failed: %v", mode, err)
		}
		if got != "three" {
			t.Fatalf("mode %o: got %q", mode, got)
		}
		info, err := p.Stat(path)
		if err != nil || !info.IsFile {
			t.Fatalf("mode %o: stat = %#v, %v", mode, info, err)
		}
	}
}

// AppendFile creates the file and its parents on a first call and extends it
// afterwards; both branches sync, and only the creating one syncs the parent.
func TestProviderAppendFileCreatesThenExtends(t *testing.T) {
	t.Parallel()
	_, p := newTestProvider(t, "session")
	const path = "/session-state/nested/events.jsonl"
	if err := p.AppendFile(path, "one\n", nil); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := p.AppendFile(path, "two\n", nil); err != nil {
		t.Fatalf("second append: %v", err)
	}
	got, err := p.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "one\ntwo\n" {
		t.Fatalf("got %q", got)
	}
}

func TestProviderKeepsDirectoriesUsableForRestrictiveModes(t *testing.T) {
	t.Parallel()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Mkdir(probe, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(probe, 0o700) })
	if err := os.WriteFile(filepath.Join(probe, "payload"), nil, 0o600); err == nil {
		t.Skip("filesystem does not enforce directory write permission")
	}
	manager, p := newTestProvider(t, "session")
	root := manager.SessionRoot("session")
	mode := 0o555
	if err := p.MakeDirectory("/plain", false, &mode); err != nil {
		t.Fatal(err)
	}
	if err := p.MakeDirectory("/nested/deep", true, &mode); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"plain", filepath.Join("nested", "deep")} {
		full := filepath.Join(root, dir)
		info, err := os.Stat(full)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", dir, info.Mode().Perm())
		}
		// Write directly so the assertion is not satisfied by the provider
		// re-hardening the directory on its way to the file.
		if err := os.WriteFile(filepath.Join(full, "payload"), []byte("ok"), 0o600); err != nil {
			t.Fatalf("%s is not writable: %v", dir, err)
		}
	}
}

func TestSafeSessionIDIsInjectiveForUnsafeValues(t *testing.T) {
	t.Parallel()
	if safeSessionID("a/b") == safeSessionID("a?b") {
		t.Fatal("unsafe session IDs collided")
	}
	if safeSessionID(".") == "." || safeSessionID("..") == ".." {
		t.Fatal("dot segment was preserved")
	}
}

func TestEnsureSessionCreatesReadableProviderRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := NewManager(root)
	t.Cleanup(func() { _ = m.Close() })
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

func TestManagerReopensStoreRootAfterClose(t *testing.T) {
	t.Parallel()
	m := NewManager(t.TempDir())
	if err := m.EnsureSession("session"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal("Close was not idempotent:", err)
	}
	if err := m.Provider("session").WriteFile("/events.jsonl", "ok", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerSharesLockForSessionWithoutRetainingProviders(t *testing.T) {
	t.Parallel()
	manager := NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close() })
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
	t.Parallel()
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
	t.Parallel()
	root := t.TempDir()
	path, err := WriteEvents(root, "abc", []byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("events mode = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary file left behind: %#v", entries)
	}
}
