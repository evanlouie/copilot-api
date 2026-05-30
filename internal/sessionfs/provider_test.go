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
