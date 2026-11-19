package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveBlockDoesNotFuseLinesWithoutOriginalTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	profilePath := filepath.Join(dir, ".zshrc")
	if err := os.WriteFile(profilePath, []byte("export FOO=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	block := []byte("\n" + profileBegin + "\nexport PATH=/opt/ohya:$PATH\n" + profileEnd + "\n")
	if _, err := ApplyProfile(state, Profile{Path: profilePath, Content: block}); err != nil {
		t.Fatal(err)
	}
	cur, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, append(cur, []byte("export BAR=2\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveProfiles(state); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	want := "export FOO=1\nexport BAR=2\n"
	if string(got) != want {
		t.Fatalf("profile after remove = %q, want %q", string(got), want)
	}

	tomlPath := filepath.Join(dir, "config.toml")
	receiptPath := filepath.Join(state, "codex.json")
	if err := os.WriteFile(tomlPath, []byte("model = \"gpt-5\""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTOMLEntry(tomlPath, receiptPath, Command{Command: "/usr/local/bin/ohya", Args: []string{"mcp"}}); err != nil {
		t.Fatal(err)
	}
	cur, err = os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tomlPath, append(cur, []byte("[features]\nshell_snapshot = true\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveTOMLEntry(receiptPath); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	wantTOML := "model = \"gpt-5\"\n[features]\nshell_snapshot = true\n"
	if string(got) != wantTOML {
		t.Fatalf("toml after remove = %q, want %q", string(got), wantTOML)
	}
}
