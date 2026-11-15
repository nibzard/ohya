package setup

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServicePlans(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		s, err := ServicePlan(platform, "/home/a b", "/home/a b/ohya", []string{"ingest", "--state", "/a&b/<state>", "--codex-root", ""})
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(s.Path) || len(s.Start) == 0 || len(s.Stop) == 0 || len(s.Check) == 0 {
			t.Fatalf("incomplete plan: %+v", s)
		}
		if platform == "linux" {
			if !strings.Contains(string(s.Content), `"/home/a b/ohya" "ingest"`) || !strings.Contains(string(s.Content), `"--codex-root" ""`) {
				t.Fatal(string(s.Content))
			}
		} else {
			d := xml.NewDecoder(strings.NewReader(string(s.Content)))
			var values []string
			for {
				token, err := d.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if start, ok := token.(xml.StartElement); ok && start.Name.Local == "string" {
					var v string
					if err := d.DecodeElement(&v, &start); err != nil {
						t.Fatal(err)
					}
					values = append(values, v)
				}
			}
			if !strings.Contains(strings.Join(values, "|"), "/a&b/<state>") {
				t.Fatal(values)
			}
		}
	}
	if systemdQuote("/a%/$x") != `"/a%%/$$x"` {
		t.Fatal("systemd expansion not escaped")
	}
	if _, err := ServicePlan("linux", "/home", "/bin/ohya", []string{"bad\narg"}); err == nil {
		t.Fatal("accepted newline")
	}
	if _, err := ServicePlan("windows", "/home", "/ohya", nil); err == nil {
		t.Fatal("accepted unsupported platform")
	}
}

func TestOwnedFileAndLock(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Lock(dir); err == nil {
		second()
		t.Fatal("acquired lock twice")
	}
	unlock()
	unlock, err = Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	path, r := filepath.Join(dir, "user", "ohya.service"), filepath.Join(dir, "receipt.json")
	if changed, err := ApplyFile(path, r, []byte("first")); err != nil || !changed {
		t.Fatalf("apply: %v %v", changed, err)
	}
	if changed, err := ApplyFile(path, r, []byte("first")); err != nil || changed {
		t.Fatalf("repeat: %v %v", changed, err)
	}
	if _, err := ApplyFile(path, r, []byte("next")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveFile(r); err == nil {
		t.Fatal("removed edited file")
	}
	if _, err := ApplyFile(path, r, []byte("replace")); err == nil {
		t.Fatal("replaced edited file")
	}
	if err := os.WriteFile(path, []byte("next"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveFile(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file remains")
	}
}
