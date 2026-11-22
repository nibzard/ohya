package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func entryFixture(t *testing.T) (JSONEntry, string) {
	t.Helper()
	dir := t.TempDir()
	return JSONEntry{Path: filepath.Join(dir, "client.json"), Parent: "mcpServers", Name: "ohya", Value: json.RawMessage(`{"command":"/bin/ohya","args":["mcp"]}`)}, filepath.Join(dir, "receipts", "client.json")
}

func TestJSONEntryLifecycle(t *testing.T) {
	e, r := entryFixture(t)
	if err := os.WriteFile(e.Path, []byte(`{"model":"keep","mcpServers":{"other":{"url":"https://example.com"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := ApplyJSONEntry(e, r); err != nil || !changed {
		t.Fatalf("apply = %v, %v", changed, err)
	}
	if changed, err := ApplyJSONEntry(e, r); err != nil || changed {
		t.Fatalf("repeat = %v, %v", changed, err)
	}
	e.Value = json.RawMessage(`{"command":"/new/ohya","args":["mcp"]}`)
	if changed, err := ApplyJSONEntry(e, r); err != nil || !changed {
		t.Fatalf("upgrade = %v, %v", changed, err)
	}
	if _, err := RemoveJSONEntry(r); err != nil {
		t.Fatal(err)
	}
	root, children, _, err := readEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if string(root["model"]) != `"keep"` || children["other"] == nil || children["ohya"] != nil {
		t.Fatalf("remaining config = %v / %v", root, children)
	}
}

func TestJSONEntryPreservesForeignAndEditedEntries(t *testing.T) {
	for _, owned := range []bool{false, true} {
		e, r := entryFixture(t)
		if owned {
			if _, err := ApplyJSONEntry(e, r); err != nil {
				t.Fatal(err)
			}
		}
		before := []byte(`{"mcpServers":{"ohya":{"command":"custom"}}}`)
		if err := os.WriteFile(e.Path, before, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyJSONEntry(e, r); err == nil {
			t.Fatal("replaced foreign entry")
		}
		if owned {
			if _, err := RemoveJSONEntry(r); err == nil {
				t.Fatal("removed edited entry")
			}
		}
		after, _ := os.ReadFile(e.Path)
		if string(after) != string(before) {
			t.Fatal("changed config")
		}
	}
}

func TestJSONEntryResumesPreparedUpdate(t *testing.T) {
	e, r := entryFixture(t)
	if _, err := ApplyJSONEntry(e, r); err != nil {
		t.Fatal(err)
	}
	receipt := entryReceipt{Entry: e, Previous: e.Value}
	receipt.Entry.Value = json.RawMessage(`{"command":"/next/ohya"}`)
	raw, _ := json.Marshal(receipt)
	if err := os.WriteFile(r, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyJSONEntry(receipt.Entry, r); err != nil {
		t.Fatal(err)
	}
}

func TestJSONEntryEmptyAndWhitespaceFiles(t *testing.T) {
	for _, initial := range []string{"", " \n\t "} {
		e, r := entryFixture(t)
		if err := os.WriteFile(e.Path, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
		if changed, err := ApplyJSONEntry(e, r); err != nil || !changed {
			t.Fatalf("apply on %q = %v, %v", initial, changed, err)
		}
		if changed, err := ApplyJSONEntry(e, r); err != nil || changed {
			t.Fatalf("repeat on %q = %v, %v", initial, changed, err)
		}
		if _, err := RemoveJSONEntry(r); err != nil {
			t.Fatalf("remove on %q: %v", initial, err)
		}
	}
}

func TestJSONEntryRejectsMalformedAndSymlink(t *testing.T) {
	e, r := entryFixture(t)
	if err := os.WriteFile(e.Path, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyJSONEntry(e, r); err == nil {
		t.Fatal("accepted null")
	}
	if err := os.Remove(e.Path); err != nil {
		t.Fatal(err)
	}
	target := e.Path + ".target"
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyJSONEntry(e, r); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("symlink error = %v", err)
	}
}
