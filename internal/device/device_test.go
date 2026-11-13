package device

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateMakesStableID(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if len(id) != 32 || strings.ToLower(id) != id {
		t.Fatalf("ID = %q, want 32 lowercase hex characters", id)
	}

	// The second load returns the same ID and leaves the file untouched.
	before, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read identity file: %v", err)
	}
	again, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if again != id {
		t.Fatalf("ID changed: %q then %q", id, again)
	}
	after, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("reread identity file: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("identity file changed across loads:\n%s\n%s", before, after)
	}
	if !strings.Contains(string(after), `"device_id": "`+id+`"`) {
		t.Fatalf("identity file does not record the ID:\n%s", after)
	}
}

func TestLoadOrCreateMakesDistinctIDs(t *testing.T) {
	a, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	b, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if a == b {
		t.Fatalf("two devices share ID %q", a)
	}
}

func TestLoadOrCreateCreatesNestedStateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ohya", "state")
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, FileName)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("identity file missing under %s: %v", dir, err)
	}
}

func TestLoadOrCreateRejectsUnusableIdentityFile(t *testing.T) {
	cases := map[string]string{
		"corrupt JSON":    "{not json",
		"wrong version":   `{"version":2,"device_id":"0123456789abcdef0123456789abcdef"}`,
		"missing ID":      `{"version":1}`,
		"malformed ID":    `{"version":1,"device_id":"not-hex"}`,
		"truncated ID":    `{"version":1,"device_id":"0123"}`,
		"non-object JSON": `["version"]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write identity file: %v", err)
			}
			id, err := LoadOrCreate(dir)
			if !errors.Is(err, ErrUnusable) {
				t.Fatalf("error = %v, want ErrUnusable", err)
			}
			if id != "" {
				t.Fatalf("ID = %q, want none", id)
			}
			// The unusable file must stay untouched: no silent regeneration.
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != content {
				t.Fatalf("identity file was modified: %v", readErr)
			}
		})
	}
}
