package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/watch"
)

func TestJoinCodeEnvValidation(t *testing.T) {
	t.Setenv("OHYA_EMPTY_JOIN_CODE", "")
	t.Setenv("OHYA_BAD_JOIN_CODE", "not-a-valid-code")
	t.Setenv("OHYA_GOOD_JOIN_CODE", "ohya1.synthetic-token.tcaddr")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"both", []string{"join", "ohya1.a.b", "--join-code-env", "OHYA_GOOD_JOIN_CODE"}, "not both"},
		{"bad name", []string{"join", "--join-code-env", "1BAD"}, "invalid join code environment variable name"},
		{"empty", []string{"join", "--join-code-env", "OHYA_EMPTY_JOIN_CODE"}, "join code environment variable is empty"},
		{"invalid code", []string{"join", "--join-code-env", "OHYA_BAD_JOIN_CODE"}, "join code environment variable does not hold one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI(tc.args...)
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("args %v -> code %d stderr %q, want 2 with %q", tc.args, code, stderr, tc.want)
			}
		})
	}
}

func TestReportJoinBacklogClampsTruncatedFile(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	srcDir := filepath.Join(tmp, "claude")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(srcDir, "sess.jsonl")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 200), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := watch.New(stateDir, "claude-code", watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := w.Poll([]string{path})
	if err != nil || len(outcomes) != 1 {
		t.Fatalf("Poll: %v (%d)", err, len(outcomes))
	}
	if err := w.Acknowledge(path, outcomes[0].SourceID, 200); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	reportJoinBacklog(stateDir, []agentRoot{{agent: "claude-code", dirs: []agentDir{{dir: srcDir}}}}, &stderr)
	if !strings.Contains(stderr.String(), "upload backlog: 0 bytes") {
		t.Fatalf("reportJoinBacklog = %q, want 0 bytes", stderr.String())
	}
}
