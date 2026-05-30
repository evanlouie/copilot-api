package sessionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

const SessionStatePath = "/session-state"

type Manager struct {
	Root string
	mu   sync.Mutex
}

func NewManager(root string) *Manager { return &Manager{Root: root} }

func (m *Manager) SessionRoot(sessionID string) string {
	return filepath.Join(m.Root, "sessions", safeSessionID(sessionID))
}

func (m *Manager) EnsureSession(sessionID string) error {
	return os.MkdirAll(m.SessionRoot(sessionID), 0o700)
}

func (m *Manager) Provider(sessionID string) *Provider {
	return &Provider{root: m.SessionRoot(sessionID)}
}

type Provider struct {
	root string
	mu   sync.Mutex
}

func (p *Provider) ReadFile(path string) (string, error) {
	b, err := os.ReadFile(p.fullPath(path))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *Provider) WriteFile(path string, content string, mode *int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	full := p.fullPath(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	perm := os.FileMode(0o600)
	if mode != nil {
		perm = os.FileMode(*mode)
	}
	return os.WriteFile(full, []byte(content), perm)
}

func (p *Provider) AppendFile(path string, content string, mode *int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	full := p.fullPath(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	perm := os.FileMode(0o600)
	if mode != nil {
		perm = os.FileMode(*mode)
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

func (p *Provider) Exists(path string) (bool, error) {
	_, err := os.Stat(p.fullPath(path))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (p *Provider) Stat(path string) (*copilot.SessionFsFileInfo, error) {
	info, err := os.Stat(p.fullPath(path))
	if err != nil {
		return nil, err
	}
	ts := info.ModTime().UTC()
	return &copilot.SessionFsFileInfo{IsFile: !info.IsDir(), IsDirectory: info.IsDir(), Size: info.Size(), Mtime: ts, Birthtime: ts}, nil
}

func (p *Provider) Mkdir(path string, recursive bool, mode *int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	full := p.fullPath(path)
	perm := os.FileMode(0o700)
	if mode != nil {
		perm = os.FileMode(*mode)
	}
	if recursive {
		return os.MkdirAll(full, perm)
	}
	return os.Mkdir(full, perm)
}

func (p *Provider) Readdir(path string) ([]string, error) {
	entries, err := os.ReadDir(p.fullPath(path))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func (p *Provider) ReaddirWithTypes(path string) ([]rpc.SessionFSReaddirWithTypesEntry, error) {
	entries, err := os.ReadDir(p.fullPath(path))
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

func (p *Provider) Rm(path string, recursive bool, force bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	full := p.fullPath(path)
	var err error
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
	p.mu.Lock()
	defer p.mu.Unlock()
	destPath := p.fullPath(dest)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return err
	}
	return os.Rename(p.fullPath(src), destPath)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

func safeSessionID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "session"
	}
	return b.String()
}
