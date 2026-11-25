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

const mcpTestNowMS = int64(1_800_000_000_000)

const mcpTestAlikeNote = "two uncommitted builds of one revision cannot be told apart"

func writeMCPSnapshot(t *testing.T, dir, id string, role observe.Role, pid int, revision string, dirty bool, sampledMS int64, stopped bool) {
	t.Helper()
	snapshot := observe.Snapshot{
		FormatVersion: observe.SnapshotFormatVersion,
		Instance: observe.Instance{
			InstanceID:    id,
			PID:           pid,
			Roles:         []string{string(role)},
			StartWallMS:   sampledMS - 1000,
			BuildRevision: revision,
			BuildDirty:    dirty,
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
	writeMCPSnapshot(t, dir, strings.Repeat("a", 32), observe.RoleMCP, pid, "old1234", false, mcpTestNowMS-1000, false)
	writeMCPSnapshot(t, dir, strings.Repeat("b", 32), observe.RoleMCP, pid, "new5678", false, mcpTestNowMS-1000, false)
	writeMCPSnapshot(t, dir, strings.Repeat("c", 32), observe.RoleServe, pid, "old1234", false, mcpTestNowMS-1000, false)
	findings := checkMCPBuilds(dir, "new5678", false, mcpTestNowMS)
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

func TestCheckMCPBuildsComparesUncommittedBuilds(t *testing.T) {
	pid := os.Getpid()
	cases := []struct {
		name         string
		doctorDirty  bool
		revision     string
		processDirty bool
		wantProcess  string
		wantThis     string
		wantAlike    bool
	}{
		{name: "two committed builds of one revision", revision: "new5678"},
		{name: "an uncommitted process", revision: "new5678", processDirty: true, wantProcess: "new5678 with uncommitted changes", wantThis: "new5678"},
		{name: "a committed process and an uncommitted doctor", doctorDirty: true, revision: "new5678", wantProcess: "new5678", wantThis: "new5678 with uncommitted changes"},
		{name: "two uncommitted builds of one revision", doctorDirty: true, revision: "new5678", processDirty: true, wantProcess: "new5678 with uncommitted changes", wantThis: "new5678 with uncommitted changes", wantAlike: true},
		{name: "an uncommitted process of another revision", doctorDirty: true, revision: "old1234", processDirty: true, wantProcess: "old1234 with uncommitted changes", wantThis: "new5678 with uncommitted changes"},
		{name: "a process without a revision", revision: "", wantProcess: "unknown", wantThis: "new5678"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMCPSnapshot(t, dir, strings.Repeat("a", 32), observe.RoleMCP, pid, tc.revision, tc.processDirty, mcpTestNowMS-1000, false)
			findings := checkMCPBuilds(dir, "new5678", tc.doctorDirty, mcpTestNowMS)
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want one", findings)
			}
			f := findings[0]
			if tc.wantProcess == "" {
				if f.Severity != sevOK || f.Detail != "1 running ohya mcp process runs this build (new5678)" {
					t.Fatalf("finding = %+v, want the ok finding of this build", f)
				}
				return
			}
			want := fmt.Sprintf("1 of 1 running ohya mcp processes runs another build than this one (%s): PID %d build %s; their agent sessions still get the query engine, redaction, and error hints of that build", tc.wantThis, pid, tc.wantProcess)
			if tc.wantAlike {
				want += "; " + mcpTestAlikeNote + ", so that process can also be this build"
			}
			if f.Check != "mcp_build" || f.Severity != sevWarn || f.Detail != want {
				t.Fatalf("finding = %+v, want a warning with the detail %q", f, want)
			}
			if !strings.Contains(f.Remedy, "/mcp") {
				t.Fatalf("remedy = %q", f.Remedy)
			}
		})
	}
}

func TestCheckMCPBuildsIgnoresStoppedAndStaleInstances(t *testing.T) {
	dir := t.TempDir()
	pid := os.Getpid()
	writeMCPSnapshot(t, dir, strings.Repeat("a", 32), observe.RoleMCP, pid, "old1234", false, mcpTestNowMS-1000, true)
	writeMCPSnapshot(t, dir, strings.Repeat("b", 32), observe.RoleMCP, pid, "old1234", false, mcpTestNowMS-observe.SnapshotStaleAge.Milliseconds()-1, false)
	writeMCPSnapshot(t, dir, strings.Repeat("c", 32), observe.RoleMCP, pid, "new5678", false, mcpTestNowMS-1000, false)
	findings := checkMCPBuilds(dir, "new5678", false, mcpTestNowMS)
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
			writeMCPSnapshot(t, observeDir, strings.Repeat("a", 32), observe.RoleMCP, os.Getpid(), "old1234", false, nowMS-1000, false)
			writeMCPSnapshot(t, observeDir, strings.Repeat("b", 32), observe.RoleMCP, os.Getpid(), "new5678", false, nowMS-1000, false)

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

func TestDoctorComparesUncommittedBuilds(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".ohya")
	if err := setup.SaveConnection(root, setup.Connection{Mode: "local"}); err != nil {
		t.Fatal(err)
	}
	observeDir := filepath.Join(root, "observe")
	writeMCPSnapshot(t, observeDir, strings.Repeat("a", 32), observe.RoleMCP, os.Getpid(), "new5678", false, time.Now().UnixMilli()-1000, false)

	var stdout, stderr bytes.Buffer
	rt := doctorRuntime{home: home, buildRevision: "new5678", buildDirty: true}
	if err := runDoctorCommandWith(nil, &stdout, &stderr, rt); err != nil {
		t.Fatalf("doctor returned %v (stdout:\n%s)", err, stdout.String())
	}
	f := soleFinding(t, parseDoctorFindings(t, stdout.String()), "mcp_build")
	if f.Severity != sevWarn || !strings.Contains(f.Detail, "another build than this one (new5678 with uncommitted changes): PID") || !strings.Contains(f.Detail, "build new5678;") {
		t.Fatalf("mcp_build finding = %+v; want a warning for the committed build", f)
	}
}

func TestCheckMCPBuildsSkipsWithoutEvidence(t *testing.T) {
	dir := t.TempDir()
	if got := checkMCPBuilds(dir, "new5678", false, mcpTestNowMS); got != nil {
		t.Fatalf("empty observe directory: findings = %+v, want none", got)
	}
	writeMCPSnapshot(t, dir, strings.Repeat("a", 32), observe.RoleMCP, os.Getpid(), "old1234", false, mcpTestNowMS-1000, false)
	if got := checkMCPBuilds(dir, "unknown", false, mcpTestNowMS); got != nil {
		t.Fatalf("unknown build revision: findings = %+v, want none", got)
	}
	if got := checkMCPBuilds(dir, "unknown", true, mcpTestNowMS); got != nil {
		t.Fatalf("unknown build revision of an uncommitted doctor: findings = %+v, want none", got)
	}
}
