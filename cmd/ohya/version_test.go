package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

// parseVersionLine splits the version line into its key=value pairs: the
// first token must be the program name, every later token exactly one
// key=value pair, and no key may repeat.
func parseVersionLine(t *testing.T, line string) map[string]string {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "ohya" {
		t.Fatalf("version line %q must start with the program name followed by key=value pairs", line)
	}
	pairs := make(map[string]string, len(fields)-1)
	for _, f := range fields[1:] {
		key, value, found := strings.Cut(f, "=")
		if !found || key == "" || value == "" {
			t.Fatalf("version token %q is not a non-empty key=value pair", f)
		}
		if _, dup := pairs[key]; dup {
			t.Fatalf("version line repeats key %q", key)
		}
		pairs[key] = value
	}
	return pairs
}

// TestVersionAliasesPrintSameLine pins the T061 acceptance: `ohya version`,
// `ohya --version`, and `ohya -version` exit 0 with nothing on stderr and
// print the same single line, whose pairs cover every key and match the
// constants they report.
func TestVersionAliasesPrintSameLine(t *testing.T) {
	var want string
	for _, args := range [][]string{{"version"}, {"--version"}, {"-version"}} {
		var stdout, stderr bytes.Buffer
		code := cliMain(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%s: exit code = %d; want 0 (stderr: %q)", args, code, stderr.String())
		}
		if stderr.Len() > 0 {
			t.Fatalf("%s: stderr = %q; want empty", args, stderr.String())
		}
		got := stdout.String()
		if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
			t.Fatalf("%s: output %q is not exactly one line", args, got)
		}
		if want == "" {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("%s: output %q differs from the first spelling's %q", args, got, want)
		}
	}

	schema, err := store.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	pairs := parseVersionLine(t, want)
	for key, value := range map[string]string{
		"version":       "",
		"revision":      "",
		"dirty":         "",
		"go":            "",
		"schema":        strconv.Itoa(schema),
		"parser.claude": strconv.Itoa(claudecode.OutputVersion),
		"parser.codex":  strconv.Itoa(codex.OutputVersion),
		"parser.pi":     strconv.Itoa(pi.OutputVersion),
	} {
		got, ok := pairs[key]
		if !ok {
			t.Fatalf("version line %q lacks key %q", strings.TrimSpace(want), key)
		}
		if value != "" && got != value {
			t.Fatalf("version line reports %s=%s; want %s", key, got, value)
		}
		delete(pairs, key)
	}
	for key := range pairs {
		t.Fatalf("version line carries unexpected key %q", key)
	}
}

// TestVersionRejectsArguments pins the usage contract for the version
// command: an extra argument or flag is a usage error (exit 2), not a
// silently ignored one.
func TestVersionRejectsArguments(t *testing.T) {
	for _, args := range [][]string{{"version", "extra"}, {"--version", "extra"}} {
		var stdout, stderr bytes.Buffer
		code := cliMain(args, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("%s: exit code = %d; want 2 (stderr: %q)", args, code, stderr.String())
		}
		if stdout.Len() > 0 {
			t.Fatalf("%s: stdout = %q; want empty", args, stdout.String())
		}
		if !strings.Contains(stderr.String(), `unexpected argument "extra"`) {
			t.Fatalf("%s: stderr = %q; want the unexpected-argument message", args, stderr.String())
		}
	}
}
