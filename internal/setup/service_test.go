package setup

import (
	"context"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestServicePlans(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		s, err := ServicePlan(platform, "/home/a b", "/a&b/<state>", "/home/a b/ohya", []string{"ingest", "--state", "/a&b/<state>", "--codex-root", ""})
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(s.Path) || len(s.Start) == 0 || len(s.Stop) == 0 || len(s.Check) == 0 {
			t.Fatalf("incomplete plan: %+v", s)
		}
		if platform == "linux" {
			if !strings.Contains(string(s.Content), "WorkingDirectory=/home/a b\n") {
				t.Fatal("systemd directory paths must not use argument quoting")
			}
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
	if _, err := ServicePlan("linux", "/home", "/state", "/bin/ohya", []string{"bad\narg"}); err == nil {
		t.Fatal("accepted newline")
	}
	if _, err := ServicePlan("windows", "/home", "/state", "/ohya", nil); err == nil {
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

func TestSystemdAcceptsGeneratedUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd parser is Linux-only")
	}
	analyzer, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	home := filepath.Join(t.TempDir(), "home with spaces")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := ServicePlan("linux", home, filepath.Join(home, ".ohya"), "/bin/true", []string{"argument with spaces", "$HOME", "%literal", ""})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ohya.service")
	if err := os.WriteFile(path, s.Content, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, analyzer, "--user", "verify", path).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd rejected generated unit: %v %s", err, output)
	}
}

func fakeLaunchctl(t *testing.T, printsBeforeRelease int) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
dir="` + dir + `"
case "$1" in
bootout) echo ` + strconv.Itoa(printsBeforeRelease) + ` > "$dir/remaining" ;;
print)
	remaining=$(cat "$dir/remaining" 2>/dev/null || echo 0)
	echo print >> "$dir/prints"
	if [ "$remaining" -gt 0 ]; then
		echo $((remaining - 1)) > "$dir/remaining"
		echo "state = running"
		exit 0
	fi
	echo "Could not find service" >&2
	exit 113 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestLaunchdBootoutWaitsUntilLabelIsReleased(t *testing.T) {
	dir := fakeLaunchctl(t, 3)
	if err := RunServiceCommand(context.Background(), []string{"launchctl", "bootout", "gui/501/com.nibzard.ohya"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "prints"))
	if got := strings.Count(string(raw), "print"); got != 4 {
		t.Fatalf("bootout returned after %d print checks, want 4 (3 loaded, 1 released)", got)
	}
}

func TestLaunchdBootoutReportsALabelThatStaysLoaded(t *testing.T) {
	fakeLaunchctl(t, 1000)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RunServiceCommand(ctx, []string{"launchctl", "bootout", "gui/501/com.nibzard.ohya"})
	if err == nil || !strings.Contains(err.Error(), "still holds gui/501/com.nibzard.ohya") {
		t.Fatalf("bootout error = %v", err)
	}
}
