package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTOMLEntryPreservesConfiguration(t *testing.T) {
	dir := t.TempDir()
	path, r := filepath.Join(dir, "config.toml"), filepath.Join(dir, "receipt.json")
	original := "# keep this comment\nmodel = 'custom'\n[mcp_servers.other]\ncommand = 'other'\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	c := Command{Command: "/path with spaces/ohya", Args: []string{"mcp", "--state", "/a b"}}
	if changed, err := ApplyTOMLEntry(path, r, c); err != nil || !changed {
		t.Fatalf("apply = %v, %v", changed, err)
	}
	if changed, err := ApplyTOMLEntry(path, r, c); err != nil || changed {
		t.Fatalf("repeat = %v, %v", changed, err)
	}
	c.Command = "/new/ohya"
	if changed, err := ApplyTOMLEntry(path, r, c); err != nil || !changed {
		t.Fatalf("upgrade = %v, %v", changed, err)
	}
	if _, err := RemoveTOMLEntry(r); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != original {
		t.Fatalf("restored = %q, %v", after, err)
	}
}

func TestTOMLEntryRejectsForeignAndEdited(t *testing.T) {
	for _, owned := range []bool{false, true} {
		dir := t.TempDir()
		path, r := filepath.Join(dir, "config.toml"), filepath.Join(dir, "receipt.json")
		if owned {
			if _, err := ApplyTOMLEntry(path, r, Command{Command: "ohya"}); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("enabled = false\n")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("[mcp_servers.ohya]\ncommand='mine'\n"), 0600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		if _, err := ApplyTOMLEntry(path, r, Command{Command: "next"}); err == nil {
			t.Fatal("overwrote entry")
		}
		if owned {
			if _, err := RemoveTOMLEntry(r); err == nil {
				t.Fatal("removed edited entry")
			}
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("changed foreign content")
		}
	}
}
