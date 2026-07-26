package sessionfs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

const SessionStatePath = "/session-state"

const sessionLockStripes = 64

// Session trees are always owner-only. Caller-supplied modes are folded into
// these rather than merely masked against them: masking alone can produce modes
// (0o400 for a file, 0o500 for a directory) that permanently break every later
// write, because the owner is subject to the owner-class permission bits.
const (
	fileMode os.FileMode = 0o600
	dirMode  os.FileMode = 0o700
)

type Manager struct {
	Root  string
	locks [sessionLockStripes]sync.RWMutex

	storeMu   sync.Mutex
	storeRoot *os.Root
}

func NewManager(root string) *Manager { return &Manager{Root: root} }

// store returns the descriptor for the manager directory. Every path operation
// is resolved relative to it, so containment is enforced by the kernel
// (openat2/RESOLVE_BENEATH on Linux) instead of by a racy check-then-open walk.
func (m *Manager) store() (*os.Root, error) {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()
	if m.storeRoot != nil {
		return m.storeRoot, nil
	}
	if err := os.MkdirAll(m.Root, dirMode); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(m.Root)
	if err != nil {
		return nil, err
	}
	m.storeRoot = root
	return root, nil
}

// Close releases the store descriptor. It is safe to call more than once and at
// any point in the process lifetime: a later operation transparently reopens
// the store root.
func (m *Manager) Close() error {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()
	root := m.storeRoot
	m.storeRoot = nil
	if root == nil {
		return nil
	}
	return root.Close()
}

func sessionRelative(sessionID string) string {
	return filepath.Join("sessions", safeSessionID(sessionID))
}

func (m *Manager) SessionRoot(sessionID string) string {
	return filepath.Join(m.Root, sessionRelative(sessionID))
}

func (m *Manager) EnsureSession(sessionID string) error {
	store, err := m.store()
	if err != nil {
		return err
	}
	return secureDirectory(store, sessionRelative(sessionID))
}

func (m *Manager) Provider(sessionID string) *Provider {
	rel := sessionRelative(sessionID)
	return &Provider{manager: m, rel: rel, sharedMu: &m.locks[sessionLockIndex(rel)]}
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
	manager  *Manager
	rel      string
	sharedMu *sync.RWMutex
}

func (p *Provider) mutex() *sync.RWMutex { return p.sharedMu }

// session opens a root confined to the session directory, so a symlink planted
// under it cannot redirect an operation to another session or outside the store
// at all. The caller must close the returned root: the SDK never closes a
// provider, so caching the descriptor here would leak one open directory per
// session for the lifetime of the process.
func (p *Provider) session(create bool) (*os.Root, error) {
	store, err := p.manager.store()
	if err != nil {
		return nil, err
	}
	if create {
		if err := secureDirectory(store, p.rel); err != nil {
			return nil, err
		}
	}
	return store.OpenRoot(p.rel)
}

// resolve converts a request path into a session-relative name and opens the
// session root it must be resolved against.
func (p *Provider) resolve(path string, create bool) (*os.Root, string, error) {
	name, err := relativePath(path)
	if err != nil {
		return nil, "", err
	}
	root, err := p.session(create)
	if err != nil {
		return nil, "", err
	}
	return root, name, nil
}

func closeRoot(root *os.Root) { _ = root.Close() }

func (p *Provider) ReadFile(path string) (string, error) {
	mu := p.mutex()
	mu.RLock()
	defer mu.RUnlock()
	root, name, err := p.resolve(path, false)
	if err != nil {
		return "", err
	}
	defer closeRoot(root)
	b, err := root.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *Provider) WriteFile(path string, content string, mode *int) error {
	mu := p.mutex()
	mu.Lock()
	defer mu.Unlock()
	root, name, err := p.resolve(path, true)
	if err != nil {
		return err
	}
	defer closeRoot(root)
	if err := secureParent(root, name); err != nil {
		return err
	}
	return writeFileAtomic(root, name, []byte(content), hardenFileMode(mode))
}

// AppendFile is how the Copilot CLI extends a session's event log, so it is
// fsynced for the same reason WriteFile is atomic: a Responses continuation
// resumes the SDK session named by the previous record and lets the CLI reread
// that log, and only falls back to rehydrating from the session store when the
// resume fails. A silently short log is therefore the bad case - the resume
// succeeds and the model answers without the turn the client just referenced -
// so the suffix has to be as durable as the prefix WriteEvents already syncs.
//
// The cost is one durability barrier per append RPC. Appends are event-log
// writes issued while a turn runs, so their number is bounded by the turn's
// event count and the total is immaterial next to model latency.
func (p *Provider) AppendFile(path string, content string, mode *int) error {
	mu := p.mutex()
	mu.Lock()
	defer mu.Unlock()
	root, name, err := p.resolve(path, true)
	if err != nil {
		return err
	}
	defer closeRoot(root)
	if err := secureParent(root, name); err != nil {
		return err
	}
	// Checked under the session lock, which every provider operation holds, so
	// the answer still describes this call by the time the file is opened.
	_, statErr := root.Stat(name)
	created := errors.Is(statErr, fs.ErrNotExist)
	perm := hardenFileMode(mode)
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
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
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !created {
		return nil
	}
	// A first append also creates the name, and a file's own fsync does not make
	// its directory entry durable.
	dir := filepath.Dir(name)
	if err := syncDirectory(root, dir); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}

func (p *Provider) Exists(path string) (bool, error) {
	mu := p.mutex()
	mu.RLock()
	defer mu.RUnlock()
	root, name, err := p.resolve(path, false)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer closeRoot(root)
	if _, err := root.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (p *Provider) Stat(path string) (*copilot.SessionFSFileInfo, error) {
	mu := p.mutex()
	mu.RLock()
	defer mu.RUnlock()
	root, name, err := p.resolve(path, false)
	if err != nil {
		return nil, err
	}
	defer closeRoot(root)
	info, err := root.Stat(name)
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
	root, name, err := p.resolve(path, true)
	if err != nil {
		return err
	}
	defer closeRoot(root)
	perm := hardenDirMode(mode)
	if recursive {
		if err := root.MkdirAll(name, perm); err != nil {
			return err
		}
	} else if err := root.Mkdir(name, perm); err != nil {
		return err
	}
	return chmodDirectory(root, name, perm)
}

// hardenFileMode forces owner read/write on top of the requested mode so a
// caller can never create a file it is subsequently unable to read or rewrite.
func hardenFileMode(mode *int) os.FileMode {
	if mode == nil {
		return fileMode
	}
	return (os.FileMode(*mode) & fileMode) | fileMode
}

// hardenDirMode is hardenFileMode for directories, which additionally need the
// owner execute bit to be traversable.
func hardenDirMode(mode *int) os.FileMode {
	if mode == nil {
		return dirMode
	}
	return (os.FileMode(*mode) & dirMode) | dirMode
}

func secureDirectory(root *os.Root, name string) error {
	if err := root.MkdirAll(name, dirMode); err != nil {
		return err
	}
	return chmodDirectory(root, name, dirMode)
}

// secureParent hardens the directory that holds name. The session root itself
// is already hardened by session, so a name directly under it needs no work.
func secureParent(root *os.Root, name string) error {
	dir := filepath.Dir(name)
	if dir == "." {
		return nil
	}
	return secureDirectory(root, dir)
}

// chmodDirectory hardens a directory through its own descriptor. Root.Chmod
// resolves the name again and is documented as racy against a concurrent
// symlink swap on Unix; fchmod on an open directory cannot be redirected.
func chmodDirectory(root *os.Root, name string, perm os.FileMode) error {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	if err := dir.Chmod(perm); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func (p *Provider) ReadDirectory(path string) ([]string, error) {
	mu := p.mutex()
	mu.RLock()
	defer mu.RUnlock()
	root, name, err := p.resolve(path, false)
	if err != nil {
		return nil, err
	}
	defer closeRoot(root)
	entries, err := readDirectory(root, name)
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
	mu := p.mutex()
	mu.RLock()
	defer mu.RUnlock()
	root, name, err := p.resolve(path, false)
	if err != nil {
		return nil, err
	}
	defer closeRoot(root)
	entries, err := readDirectory(root, name)
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

// readDirectory lists a directory inside root. os.Root has no ReadDir method,
// so the entries are read through the directory handle and sorted by name to
// match what os.ReadDir returns.
func readDirectory(root *os.Root, name string) ([]os.DirEntry, error) {
	dir, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(-1)
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

func (p *Provider) Remove(path string, recursive bool, force bool) error {
	mu := p.mutex()
	mu.Lock()
	defer mu.Unlock()
	root, name, err := p.resolve(path, false)
	if err != nil {
		if force && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer closeRoot(root)
	if recursive {
		err = root.RemoveAll(name)
	} else {
		err = root.Remove(name)
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
	srcName, err := relativePath(src)
	if err != nil {
		return err
	}
	destName, err := relativePath(dest)
	if err != nil {
		return err
	}
	root, err := p.session(true)
	if err != nil {
		return err
	}
	defer closeRoot(root)
	if err := secureParent(root, destName); err != nil {
		return err
	}
	return root.Rename(srcName, destName)
}

// relativePath converts a request path into a name that can be resolved inside
// an os.Root. A ".." component is rejected rather than silently dropped:
// quietly operating on a different file than the caller asked for is worse than
// failing the request.
func relativePath(path string) (string, error) {
	clean := strings.TrimPrefix(filepath.ToSlash(path), "/")
	parts := strings.Split(clean, "/")
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("session path escapes root: %s", path)
		}
		filtered = append(filtered, part)
	}
	if len(filtered) == 0 {
		return ".", nil
	}
	return filepath.Join(filtered...), nil
}

// writeFileAtomic replaces name with content so a concurrent reader observes
// either the previous file or the complete new one, never a truncated prefix,
// and so an interrupted write cannot leave truncated session state behind.
func writeFileAtomic(root *os.Root, name string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(name)
	f, tmp, err := createTemp(root, dir, perm)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := root.Rename(tmp, name); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}
	if err := syncDirectory(root, dir); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}

// createTemp is os.CreateTemp for a path inside an os.Root, which has no
// CreateTemp method of its own.
func createTemp(root *os.Root, dir string, perm os.FileMode) (*os.File, string, error) {
	for range 10000 {
		name := filepath.Join(dir, ".copilot-api-"+strconv.FormatUint(rand.Uint64(), 36)+".tmp")
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return f, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("create temporary file: %w", err)
		}
	}
	return nil, "", fmt.Errorf("create temporary file in %s: %w", dir, os.ErrExist)
}

func WriteEvents(root string, sessionID string, content []byte) (string, error) {
	if err := os.MkdirAll(root, dirMode); err != nil {
		return "", fmt.Errorf("create session store: %w", err)
	}
	store, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("open session store: %w", err)
	}
	defer closeRoot(store)
	dir := filepath.Join(sessionRelative(sessionID), filepath.FromSlash(strings.TrimPrefix(SessionStatePath, "/")))
	if err := secureDirectory(store, dir); err != nil {
		return "", fmt.Errorf("create session state directory: %w", err)
	}
	name := filepath.Join(dir, "events.jsonl")
	if err := writeFileAtomic(store, name, content, fileMode); err != nil {
		return "", fmt.Errorf("write events file: %w", err)
	}
	return filepath.Join(root, name), nil
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
