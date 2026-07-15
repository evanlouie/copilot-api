package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateApplicationRoots canonicalizes roots and rejects filesystem roots,
// the home directory, the working directory, their ancestors, and overlap
// between managed roots.
func ValidateApplicationRoots(roots []string) ([]string, error) {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	protected := []string{home, cwd, string(filepath.Separator)}
	cleaned := make([]string, 0, len(roots))
	for _, root := range roots {
		abs, err := Resolve(root)
		if err != nil {
			return nil, fmt.Errorf("resolve application directory %q: %w", root, err)
		}
		for _, candidate := range protected {
			if candidate == "" {
				continue
			}
			protectedAbs, err := filepath.Abs(candidate)
			if err != nil {
				continue
			}
			protectedAbs = filepath.Clean(protectedAbs)
			if abs == protectedAbs || Contains(abs, protectedAbs) {
				return nil, fmt.Errorf("refusing unsafe application directory %s", abs)
			}
		}
		cleaned = append(cleaned, abs)
	}
	for i := range cleaned {
		for j := i + 1; j < len(cleaned); j++ {
			if cleaned[i] == cleaned[j] || Contains(cleaned[i], cleaned[j]) || Contains(cleaned[j], cleaned[i]) {
				return nil, fmt.Errorf("refusing overlapping application directories %s and %s", cleaned[i], cleaned[j])
			}
		}
	}
	return cleaned, nil
}

// Resolve canonicalizes a path through its longest existing prefix, so a
// missing leaf cannot hide a symlinked ancestor.
func Resolve(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current = filepath.Clean(current)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			resolved = filepath.Clean(resolved)
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// Contains reports whether child is strictly below parent.
func Contains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
