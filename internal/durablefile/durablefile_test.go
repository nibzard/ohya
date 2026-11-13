package durablefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := Write(path, []byte("first")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("content = %q, want %q", got, "first")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 600", perm)
	}
}

func TestWriteReplacesAtomicallyWithoutLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	for _, content := range []string{"one", "two", "three"} {
		if err := Write(path, []byte(content)); err != nil {
			t.Fatalf("Write(%q): %v", content, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "three" {
		t.Fatalf("content = %q, want %q", got, "three")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("directory holds %v, want only state.json", entries)
	}
}

func TestWriteFailsOnMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "state.json")
	if err := Write(path, []byte("x")); err == nil {
		t.Fatal("Write succeeded, want an error for a missing directory")
	}
}
