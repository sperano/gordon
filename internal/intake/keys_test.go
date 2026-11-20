package intake

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sperano/gordon/internal/event"
)

func TestFileKeysRereadsAndReportsErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vikunja")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var failed []event.Source
	keys := FileKeys(map[event.Source]string{event.SourceVikunja: path, event.SourceGitHub: filepath.Join(dir, "missing")},
		func(s event.Source, _ error) { failed = append(failed, s) })
	if k := keys(); string(k[event.SourceVikunja]) != "old" || len(k) != 1 {
		t.Fatalf("keys = %q, want the trimmed vikunja key only", k)
	}
	if len(failed) != 1 || failed[0] != event.SourceGitHub {
		t.Fatalf("errors reported for %v, want github", failed)
	}
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k := keys(); string(k[event.SourceVikunja]) != "new" {
		t.Fatalf("rotated key not picked up: %q", k[event.SourceVikunja])
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed = nil
	if k := keys(); len(k) != 0 {
		t.Fatalf("empty key file gave a key: %q", k)
	}
	if len(failed) != 1 || failed[0] != event.SourceGitHub {
		t.Fatalf("errors reported for %v, want only the unreadable github file (an empty one is no error)", failed)
	}
	if err := os.WriteFile(path, []byte("crlf\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k := keys(); string(k[event.SourceVikunja]) != "crlf" {
		t.Fatalf("CRLF key read as %q, want it trimmed", k[event.SourceVikunja])
	}
}
