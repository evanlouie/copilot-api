package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFollowsExistingSymlinkAncestorForMissingLeaf(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolved, err := Resolve(filepath.Join(link, "missing", "leaf"))
	if err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedTarget, "missing", "leaf")
	if resolved != want {
		t.Fatalf("Resolve = %q, want %q", resolved, want)
	}
}
