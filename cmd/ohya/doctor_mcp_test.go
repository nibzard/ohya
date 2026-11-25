package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/setup"
)

const mcpTestNowMS = int64(1_800_000_000_000)

func writeMCPSnapshot(t *testing.T, dir, id string, role observe.Role, pid int, revision string, sampledMS int64, stopped bool) {
	t.Helper()
	snapshot := observe.Snapshot{
		FormatVersion: observe.SnapshotFormatVersion,
		Instance: observe.Instance{
			InstanceID:    id,
			PID:           pid,
			Roles:         []string{string(role)},
			StartWallMS:   sampledMS - 1000,
			BuildRevision: revision,
			Scope:         dir,
		},
		SampledWallMS: sampledMS,
		Stopped:       stopped,
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	instanceDir := filepath.Join(dir, id)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMCPBuildsWarnsOnAnotherBuild(t *testing.T) {
	dir := t.TempDir()
	pid := os.Getpid()
	writeMCPSnapshot(t, dir, strings.Repeat("a", 32), observe.RoleMCP, pid, "old1234", mcpTestNowMS-1000, false)
	writeMCPSnapshot(t, dir, strings.Repeat("b", 32), observe.RoleMCP, pid, "new5678", mcpTestNowMS-1000, false)
	writeMCPSnapshot(t, dir, strings.Repeat("c", 32), observe.RoleServe, pid, "old1234", mcpTestNowMS-1000, false)
	findings := checkMCPBuilds(dir, "new5678", mcpTestNowMS)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	f := findings[0]
	if f.Check != "mcp_build" || f.Severity != sevWarn {
		t.Fatalf("finding = %+v, want an mcp_build warning", f)
	}
	if !strings.Contains(f.Detail, "1 of 2 running ohya mcp processes runs another build") || !strings.Contains(f.Detail, "build old1234") {
		t.Fatalf("detail = %q", f.Detail)
	}
	if !strings.Contains(f.Remedy, "/mcp") {
		t.Fatalf("remedy = %q", f.Remedy)
	}
}

func TestCheckMCPBuildsIgnoresStoppedAndStaleInstances(t *testing.T) {
	dir := t.TempDir()
	pid := os.Getpid()
	writeMCPSnapshot(t, dir, strings.Repeat("a", 32), observe.RoleMCP, pid, "old1234", mcpTestNowMS-1000, true)
	writeMCPSnapshot(t, dir, strings.Repeat("b", 32), observe.RoleMCP, pid, "old1234", mcpTestNowMS-observe.SnapshotStaleAge.Milliseconds()-1, false)
	writeMCPSnapshot(t, dir, strings.Repeat("c", 32), observe.RoleMCP, pid, "new5678", mcpTestNowMS-1000, false)
	findings := checkMCPBuilds(dir, "new5678", mcpTestNowMS)
	if len(findings) != 1 || findings[0].Severity != sevOK || !strings.Contains(findings[0].Detail, "1 running ohya mcp process runs this build") {
		t.Fatalf("findings = %+v, want one ok finding for the live instance", findings)
	}
}

func TestDoctorReportsTheMCPServersOfItsStateDirectory(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		joined bool
	}{
		{mode: "local"},
		{mode: "remote", joined: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".ohya")
			if err := setup.SaveConnection(root, setup.Connection{Mode: tc.mode}); err != nil {
				t.Fatal(err)
			}
			stateDir := root
			if tc.joined {
				stateDir = filepath.Join(root, "join")
			}
			nowMS := time.Now().UnixMilli()
			observeDir := filepath.Join(stateDir, "observe")
			writeMCPSnapshot(t, observeDir, strings.Repeat("a", 32), observe.RoleMCP, os.Getpid(), "old1234", nowMS-1000, false)
			writeMCPSnapshot(t, observeDir, strings.Repeat("b", 32), observe.RoleMCP, os.Getpid(), "new5678", nowMS-1000, false)

			var stdout, stderr bytes.Buffer
			if err := runDoctorCommandWith(nil, &stdout, &stderr, doctorRuntime{home: home, buildRevision: "new5678"}); err != nil {
				t.Fatalf("doctor returned %v (stdout:\n%s)", err, stdout.String())
			}
			f := soleFinding(t, parseDoctorFindings(t, stdout.String()), "mcp_build")
			if f.Severity != sevWarn || !strings.Contains(f.Detail, "1 of 2 running ohya mcp processes runs another build") || !strings.Contains(f.Detail, "build old1234") {
				t.Fatalf("mcp_build finding = %+v; want a warning for the old build", f)
			}
		})
	}
}

func TestCheckMCPBuildsSkipsWithoutEvidence(t *testing.T) {
	dir := t.TempDir()
	if got := checkMCPBuilds(dir, "new5678", mcpTestNowMS); got != nil {
		t.Fatalf("empty observe directory: findings = %+v, want none", got)
	}
	writeMCPSnapshot(t, dir, strings.Repeat("a", 32), observe.RoleMCP, os.Getpid(), "old1234", mcpTestNowMS-1000, false)
	if got := checkMCPBuilds(dir, "unknown", mcpTestNowMS); got != nil {
		t.Fatalf("unknown build revision: findings = %+v, want none", got)
	}
}
