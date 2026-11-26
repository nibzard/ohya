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

func TestTOMLEntryAcceptsReformattedOwnedEntry(t *testing.T) {
	dir := t.TempDir()
	path, r := filepath.Join(dir, "config.toml"), filepath.Join(dir, "receipt.json")
	c := Command{Command: "/Users/dev/.local/bin/ohya", Args: []string{"mcp", "--remote"}}
	if err := os.WriteFile(path, []byte("model = 'custom'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTOMLEntry(path, r, c); err != nil {
		t.Fatal(err)
	}
	reformatted := "model = \"custom\"\n\n" +
		"[ mcp_servers.\"ohya\" ] # owned by ohya\n" +
		"command = \"/Users/dev/.local/bin/ohya\"\n" +
		"args = [\"mcp\", \"--remote\"]\n\n" +
		"[notice.model_migrations]\n\"gpt-5.3-codex\" = \"gpt-5.4\"\n"
	if err := os.WriteFile(path, []byte(reformatted), 0600); err != nil {
		t.Fatal(err)
	}
	c.Command = "/new/ohya"
	if changed, err := ApplyTOMLEntry(path, r, c); err != nil || !changed {
		t.Fatalf("apply over reformatted entry = %v, %v", changed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "model = \"custom\"\n\n[notice.model_migrations]\n\"gpt-5.3-codex\" = \"gpt-5.4\"\n" +
		"\n[mcp_servers.ohya]\ncommand = '/new/ohya'\nargs = ['mcp', '--remote']\n"
	if string(after) != want {
		t.Fatalf("config = %q, want %q", after, want)
	}
	if changed, err := ApplyTOMLEntry(path, r, c); err != nil || changed {
		t.Fatalf("repeat = %v, %v", changed, err)
	}
}

func TestTOMLEntryRemovesReformattedOwnedEntry(t *testing.T) {
	dir := t.TempDir()
	path, r := filepath.Join(dir, "config.toml"), filepath.Join(dir, "receipt.json")
	c := Command{Command: "ohya", Args: []string{"mcp"}}
	if _, err := ApplyTOMLEntry(path, r, c); err != nil {
		t.Fatal(err)
	}
	reformatted := "[mcp_servers.ohya]\ncommand = \"ohya\"\nargs = [\"mcp\"]\n\n[mcp_servers.other]\ncommand = \"other\"\n"
	if err := os.WriteFile(path, []byte(reformatted), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveTOMLEntry(r); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[mcp_servers.other]\ncommand = \"other\"\n"; string(after) != want {
		t.Fatalf("config = %q, want %q", after, want)
	}
}

func TestTOMLEntryRejectsReformattedEntryWithOtherValues(t *testing.T) {
	dir := t.TempDir()
	path, r := filepath.Join(dir, "config.toml"), filepath.Join(dir, "receipt.json")
	if _, err := ApplyTOMLEntry(path, r, Command{Command: "ohya", Args: []string{"mcp"}}); err != nil {
		t.Fatal(err)
	}
	edited := "[mcp_servers.ohya]\ncommand = \"ohya\"\nargs = [\"mcp\", \"--debug\"]\n"
	if err := os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTOMLEntry(path, r, Command{Command: "next"}); err == nil {
		t.Fatal("overwrote an edited entry")
	}
	after, _ := os.ReadFile(path)
	if string(after) != edited {
		t.Fatalf("changed edited content: %q", after)
	}
}
