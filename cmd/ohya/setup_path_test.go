package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupReportsPathConflict(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "ohya")
	other := filepath.Join(t.TempDir(), "ohya")
	for _, path := range []string{current, other} {
		if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if got := setupPathConflict(current); got != nil {
		t.Fatal(got)
	}
	got := setupPathConflict(other)
	if got == nil || got.Status != "warning" || !strings.Contains(got.Detail, current) || !strings.Contains(got.Detail, other) {
		t.Fatalf("conflict %+v", got)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, current); err != nil {
		t.Fatal(err)
	}
	if got := setupPathConflict(other); got != nil {
		t.Fatal("reported equivalent executable")
	}
}
