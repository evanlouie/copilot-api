package sessionstore

import "path/filepath"

// LifecycleLockPath is outside purgeable roots. Holding this lock prevents a
// new server from creating a fresh state-directory lock while purge removes the
// old state directory and its locked inode.
func LifecycleLockPath(configDir string) string {
	return filepath.Join(configDir, "lifecycle.lock")
}
