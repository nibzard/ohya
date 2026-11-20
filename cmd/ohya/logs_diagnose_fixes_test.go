package main

import (
	"bytes"
	"context"
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

func seedObservedInstance(t *testing.T, stateDir, id string, startedMS int64, entries []observe.LogEntry) {
	t.Helper()
	obsDir := filepath.Join(stateDir, "observe")
	instDir, err := observe.EnsureInstanceDir(obsDir, id)
	if err != nil {
		t.Fatal(err)
	}
	snap := observe.Snapshot{
		FormatVersion: observe.SnapshotFormatVersion,
		Instance: observe.Instance{
			InstanceID:  id,
			Roles:       []string{string(observe.RoleIngest)},
			PID:         os.Getpid(),
			StartWallMS: startedMS,
		},
		SampledWallMS: startedMS + 1000,
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		var buf bytes.Buffer
		for _, e := range entries {
			line, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(instDir, "events.jsonl"), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecentWarningEntriesKeepsNewestAcrossInstances(t *testing.T) {
	stateDir := t.TempDir()
	id1 := "00000000000000000000000000000001"
	id2 := "00000000000000000000000000000002"
	seedObservedInstance(t, stateDir, id1, 1000, []observe.LogEntry{
		{FormatVersion: 1, TimeMS: 1000, InstanceID: id1, Component: "ingest", Level: observe.LevelWarn, EventCode: string(observe.EventCommitFailed)},
		{FormatVersion: 1, TimeMS: 2000, InstanceID: id1, Component: "ingest", Level: observe.LevelWarn, EventCode: string(observe.EventCommitFailed)},
	})
	seedObservedInstance(t, stateDir, id2, 5000, []observe.LogEntry{
		{FormatVersion: 1, TimeMS: 5000, InstanceID: id2, Component: "ingest", Level: observe.LevelError, EventCode: string(observe.EventCommitFailed)},
		{FormatVersion: 1, TimeMS: 6000, InstanceID: id2, Component: "ingest", Level: observe.LevelError, EventCode: string(observe.EventCommitFailed)},
	})

	got := recentWarningEntries(stateDir, 2)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].TimeMS != 5000 || got[1].TimeMS != 6000 {
		t.Fatalf("got timestamps [%d, %d], want newest [5000, 6000]", got[0].TimeMS, got[1].TimeMS)
	}
}

func TestLogsExplicitInstanceBypassesMaxLiveInstancesCap(t *testing.T) {
	stateDir := t.TempDir()
	target := fmt.Sprintf("%032x", 1)
	for i := 1; i <= observe.MaxLiveInstances+2; i++ {
		id := fmt.Sprintf("%032x", i)
		seedObservedInstance(t, stateDir, id, int64(i*1000), []observe.LogEntry{
			{FormatVersion: 1, TimeMS: time.Now().UnixMilli(), InstanceID: id, Component: "ingest", Level: observe.LevelWarn, EventCode: string(observe.EventCommitFailed)},
		})
	}
	var stdout, stderr bytes.Buffer
	err := runLogs(context.Background(), logsOptions{
		stateDir:   stateDir,
		instanceID: target,
		since:      time.Hour,
		limit:      10,
		nowMS:      func() int64 { return time.Now().UnixMilli() },
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runLogs --instance with >MaxLiveInstances failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "commit_failed") {
		t.Fatalf("stdout missing expected event: %q", stdout.String())
	}
}

func TestResolveObserveStateDirRemote(t *testing.T) {
	setupDir := t.TempDir()
	dbPath := filepath.Join(setupDir, "hub.db")
	if err := setup.SaveConnection(setupDir, setup.Connection{Mode: "remote"}); err != nil {
		t.Fatal(err)
	}
	gotState, gotSetup, err := resolveObserveStateDir("", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wantJoin := filepath.Join(setupDir, "join")
	if gotState != wantJoin || gotSetup != setupDir {
		t.Fatalf("resolveObserveStateDir = (%q, %q), want (%q, %q)", gotState, gotSetup, wantJoin, setupDir)
	}
	explicit := filepath.Join(setupDir, "custom")
	gotExplicit, _, err := resolveObserveStateDir(explicit, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotExplicit != explicit {
		t.Fatalf("explicit state = %q, want %q", gotExplicit, explicit)
	}
}

func TestParseInstallArgsEqualsSyntax(t *testing.T) {
	dir, version, dry, setupArgs, err := parseInstallArgs(
		[]string{"--bin-dir=/tmp/bin", "--version=v1.2.3", "--dry-run=true", "--no-service"},
		"/default/bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/tmp/bin" || version != "v1.2.3" || !dry {
		t.Fatalf("got (%q, %q, %v), want (/tmp/bin, v1.2.3, true)", dir, version, dry)
	}
	if len(setupArgs) != 2 || setupArgs[0] != "--dry-run=true" || setupArgs[1] != "--no-service" {
		t.Fatalf("setupArgs = %v", setupArgs)
	}
}
