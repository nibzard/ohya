package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/setup"
)

const setupMCPRemedy = "of that build; reconnect the ohya MCP server in each agent session that uses it (in Claude Code run /mcp) or start a new session; setup never stops coding sessions"

func writeSetupMCPSnapshots(t *testing.T, observeDir string, builds ...string) {
	t.Helper()
	for i, revision := range builds {
		writeMCPSnapshot(t, observeDir, strings.Repeat(string(rune('a'+i)), 32), observe.RoleMCP, os.Getpid(), revision, false, time.Now().UnixMilli()-1000, false)
	}
}

func setupMCPBuildResult(t *testing.T, out string) *setup.Result {
	t.Helper()
	var report setup.Report
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &report); err != nil {
		t.Fatalf("decode the setup report %s: %v", out, err)
	}
	for _, r := range report.Results {
		if r.Component == "mcp-builds" {
			return &r
		}
	}
	return nil
}

func TestSetupReportsMCPServersOfAnotherBuild(t *testing.T) {
	pid := os.Getpid()
	cases := []struct {
		name        string
		remote      bool
		setupDirty  bool
		builds      []string
		wantWarning string
	}{
		{name: "local", builds: []string{"old1234", "new5678"},
			wantWarning: fmt.Sprintf("1 of 2 running ohya mcp processes runs another build than this one (new5678): PID %d build old1234; ", pid)},
		{name: "remote", remote: true, builds: []string{"old1234", "new5678"},
			wantWarning: fmt.Sprintf("1 of 2 running ohya mcp processes runs another build than this one (new5678): PID %d build old1234; ", pid)},
		{name: "an uncommitted setup build", setupDirty: true, builds: []string{"new5678"},
			wantWarning: fmt.Sprintf("1 of 1 running ohya mcp processes runs another build than this one (new5678 with uncommitted changes): PID %d build new5678; ", pid)},
		{name: "local, each server runs this build", builds: []string{"new5678"}},
		{name: "remote, each server runs this build", remote: true, builds: []string{"new5678"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(t.TempDir(), "state")
			dataState := state
			if tc.remote {
				if err := setup.SaveConnection(state, setup.Connection{Mode: "remote"}); err != nil {
					t.Fatal(err)
				}
				dataState = filepath.Join(state, "join")
			}
			writeSetupMCPSnapshots(t, filepath.Join(dataState, "observe"), tc.builds...)

			var stdout, stderr bytes.Buffer
			args := []string{"--dry-run", "--no-agents", "--no-service", "--no-modify-path", "--state", state, "--json"}
			rt := setupRuntime{home: home, buildRevision: "new5678", buildDirty: tc.setupDirty}
			if err := runSetupCommandWith(args, &stdout, &stderr, rt); err != nil {
				t.Fatalf("setup returned %v (stdout:\n%s)", err, stdout.String())
			}
			got := setupMCPBuildResult(t, stdout.String())
			if tc.wantWarning == "" {
				if got != nil {
					t.Fatalf("mcp-builds result = %+v, want none when each server runs this build", *got)
				}
				return
			}
			if got == nil || got.Status != "warning" || got.Changed || !strings.HasPrefix(got.Detail, tc.wantWarning) || !strings.HasSuffix(got.Detail, setupMCPRemedy) {
				t.Fatalf("mcp-builds result = %+v, want a warning that starts with %q and ends with %q", got, tc.wantWarning, setupMCPRemedy)
			}
		})
	}
}

func TestSetupPrintsMCPServersOfAnotherBuildAfterARun(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	writeSetupMCPSnapshots(t, filepath.Join(state, "observe"), "old1234")
	wrapper := filepath.Join(dir, "ohya-wrapper")
	quoted := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quoted+" child-run \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"--state", state, "--binary", wrapper, "--no-agents", "--no-service", "--no-modify-path"}
	if err := runSetupCommandWith(args, &stdout, &stderr, setupRuntime{home: filepath.Join(dir, "home"), buildRevision: "new5678"}); err != nil {
		t.Fatalf("setup returned %v (stdout:\n%s)", err, stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("memory")) || !bytes.Contains(stdout.Bytes(), []byte("ready")) {
		t.Fatalf("setup output has no ready memory result:\n%s%s", stdout.String(), stderr.String())
	}
	var line string
	for _, candidate := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(candidate, "mcp-builds") {
			line = candidate
		}
	}
	if !strings.Contains(line, "▲") || !strings.Contains(line, "warning") || !strings.Contains(line, "build old1234") || !strings.Contains(stdout.String(), "/mcp") {
		t.Fatalf("mcp-builds line = %q, want a warning that names the old build; output:\n%s", line, stdout.String())
	}
}
