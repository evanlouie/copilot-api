package sessionfs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

const SessionStatePath = "/session-state"

const sessionLockStripes = 64

type Manager struct {
	Root  string
	locks [sessionLockStripes]sync.Mutex
}

func NewManager(root string) *Manager { return &Manager{Root: root} }

func (m *Manager) SessionRoot(sessionID string) string {
	return filepath.Join(m.Root, "sessions", safeSessionID(sessionID))
}

func (m *Manager) EnsureSession(sessionID string) error {
	root := m.SessionRoot(sessionID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.Chmod(root, 0o700)
}

func (m *Manager) Provider(sessionID string) *Provider {
	root := m.SessionRoot(sessionID)
	return &Provider{root: root, baseRoot: m.Root, sharedMu: &m.locks[sessionLockIndex(root)]}
}

func sessionLockIndex(root string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	hash := uint32(offset32)
	for i := 0; i < len(root); i++ {
		hash ^= uint32(root[i])
		hash *= prime32
	}
	return hash % sessionLockStripes
}

type Provider struct {
	root     string
	baseRoot string
	mu       sync.Mutex
	sharedMu *sync.Mutex
}

func (p *Provider) mutex() *sync.Mutex {
	if p.sharedMu != nil {
		return p.sharedMu
	}
	return &p.mu
}

func (p *Provider) ReadFile(path string) (string, error) {
	full, err := p.checkedPath(path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *Provider) WriteFile(path string, content string, mode *int) error {
	mu := p.mutex()
	mu.Lock()
	defer mu.Unlock()
	full, err := p.checkedPath(path)
	if err != nil {
		return err
	}
	if err := secureDirectory(filepath.Dir(full)); err != nil {
		return err
	}
	perm := os.FileMode(0o600)
	if mode != nil {
		perm = os.FileMode(*mode) & 0o600
	}
	if err := os.WriteFile(full, []byte(content), perm); err != nil {
		return err
	}
	return os.Chmod(full, perm)
}

func (p *Provider) AppendFile(path string, content string, mode *int) error {
	mu := p.mutex()
	mu.Lock()
	defer mu.Unlock()
	full, err := p.checkedPath(path)
	if err != nil {
		return err
	}
	if err := secureDirectory(filepath.Dir(full)); err != nil {
		return err
	}
	perm := os.FileMode(0o600)
	if mode != nil {
		perm = os.FileMode(*mode) & 0o600
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
	if err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (p *Provider) Exists(path string) (bool, error) {
	full, err := p.checkedPath(path)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(full)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (p *Provider) Stat(path string) (*copilot.SessionFSFileInfo, error) {
	full, err := p.checkedPath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	ts := info.ModTime().UTC()
	return &copilot.SessionFSFileInfo{IsFile: !info.IsDir(), IsDirectory: info.IsDir(), Size: info.Size(), Mtime: ts, Birthtime: ts}, nil
}

func (p *Provider) MakeDirectory(path string, recursive bool, mode *int) error {
	mu := p.mutex()
	mu.Lock()
	defer mu.Unlock()
	full, err := p.checkedPath(path)
	if err != nil {
		return err
	}
	perm := os.FileMode(0o700)
	if mode != nil {
		perm = os.FileMode(*mode) & 0o700
	}
	if recursive {
		if err := os.MkdirAll(full, perm); err != nil {
			return err
		}
		return os.Chmod(full, perm)
	}
	return os.Mkdir(full, perm)
}

func secureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func (p *Provider) ReadDirectory(path string) ([]string, error) {
	full, err := p.checkedPath(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func (p *Provider) ReadDirectoryWithTypes(path string) ([]rpc.SessionFSReaddirWithTypesEntry, error) {
	full, err := p.checkedPath(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	result := make([]rpc.SessionFSReaddirWithTypesEntry, 0, len(entries))
	for _, entry := range entries {
		entryType := rpc.SessionFSReaddirWithTypesEntryTypeFile
		if entry.IsDir() {
			entryType = rpc.SessionFSReaddirWithTypesEntryTypeDirectory
		}
		result = append(result, rpc.SessionFSReaddirWithTypesEntry{Name: entry.Name(), Type: entryType})
	}
	return result, nil
}

func (p *Provider) Remove(path string, recursive bool, force bool) error {
	mu := p.mutex()
	mu.Lock()
	defer mu.Unlock()
	full, err := p.checkedPath(path)
	if err != nil {
		return err
	}
	if recursive {
		err = os.RemoveAll(full)
	} else {
		err = os.Remove(full)
	}
	if err != nil && force && os.IsNotExist(err) {
		return nil
	}
	return err
}

func (p *Provider) Rename(src string, dest string) error {
	mu := p.mutex()
	mu.Lock()
	defer mu.Unlock()
	destPath, err := p.checkedPath(dest)
	if err != nil {
		return err
	}
	srcPath, err := p.checkedPath(src)
	if err != nil {
		return err
	}
	if err := secureDirectory(filepath.Dir(destPath)); err != nil {
		return err
	}
	return os.Rename(srcPath, destPath)
}

func (p *Provider) checkedPath(path string) (string, error) {
	full := p.fullPath(path)
	base := p.baseRoot
	if base == "" {
		base = p.root
	}
	if err := rejectSymlinkPath(base, full); err != nil {
		return "", err
	}
	return full, nil
}

func rejectSymlinkPath(base, target string) error {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("session path escapes root")
	}
	if info, err := os.Lstat(base); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session path contains symlink: %s", base)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	current := base
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("session path contains symlink: %s", current)
		}
	}
	return nil
}

func (p *Provider) fullPath(path string) string {
	clean := strings.TrimPrefix(filepath.ToSlash(path), "/")
	if clean == "" || clean == "." {
		return p.root
	}
	parts := strings.Split(clean, "/")
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		filtered = append(filtered, part)
	}
	return filepath.Join(append([]string{p.root}, filtered...)...)
}

func WriteEvents(root string, sessionID string, content []byte) (string, error) {
	path := filepath.Join(root, "sessions", safeSessionID(sessionID), strings.TrimPrefix(SessionStatePath, "/"), "events.jsonl")
	if err := rejectSymlinkPath(root, filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := rejectSymlinkPath(root, filepath.Dir(path)); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".copilot-api-events-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary events file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("secure temporary events file: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write temporary events file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("sync temporary events file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close temporary events file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("replace events file %s: %w", path, err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("sync events directory: %w", err)
	}
	return path, nil
}

func safeSessionID(id string) string {
	if id != "" && id != "." && id != ".." && !strings.HasSuffix(id, ".") && !windowsReservedName(id) {
		safe := true
		for _, r := range id {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			default:
				safe = false
			}
			if !safe {
				break
			}
		}
		if safe {
			return id
		}
	}
	return "~" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func windowsReservedName(name string) bool {
	base := strings.ToLower(strings.SplitN(name, ".", 2)[0])
	if base == "con" || base == "prn" || base == "aux" || base == "nul" {
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9'
}
