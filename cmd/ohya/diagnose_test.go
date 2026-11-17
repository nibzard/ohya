package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

func runDiagnoseForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runDiagnoseCommand(args, &stdout, &stderr)
	return stderr.String(), err
}

func TestDiagnoseExportsPrivateArtifactWithSections(t *testing.T) {
	stateDir := t.TempDir()
	id := strings.Repeat("1", 32)
	now := time.Now()
	instanceDir := seedLogInstance(t, stateDir, id, "ingest", now.UnixMilli(), []observe.LogEntry{
		logEntryAt(id, "ingest", observe.LevelInfo, observe.EventStartup, now.Add(-time.Minute)),
		logEntryAt(id, "ingest", observe.LevelWarn, observe.EventSourceStopped, now.Add(-30*time.Second)),
	})
	writeHistory(t, stateDir, id, []observe.Snapshot{historySample(now, 1000, nil)})

	output := filepath.Join(stateDir, "diag.json")
	if _, err := runDiagnoseForTest(t, "--state", stateDir, "--db", filepath.Join(stateDir, "hub.db"), "--output", output); err != nil {
		t.Fatalf("diagnose failed: %v", err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode = %o, want 0600", info.Mode().Perm())
	}
	var data diagnoseData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("artifact must decode: %v", err)
	}
	if data.FormatVersion != diagnoseFormatVersion || data.GeneratedUTC == "" {
		t.Fatalf("artifact header = %+v", data)
	}
	if data.Status == nil {
		t.Fatal("status section missing")
	}
	if len(data.Metrics) != 3 {
		t.Fatalf("metrics windows = %d, want 3: %+v", len(data.Metrics), data.Metrics)
	}
	if len(data.Logs) != 1 || data.Logs[0].EventCode != string(observe.EventSourceStopped) {
		t.Fatalf("logs section = %+v", data.Logs)
	}
	if data.Capabilities.RemoteAggregates {
		t.Fatal("remote aggregates must be reported unsupported")
	}
	if data.Limits.QueryDeadlineMS != 5000 || data.Limits.LogFileMaxBytes != observe.LogMaxFileBytes {
		t.Fatalf("limits = %+v", data.Limits)
	}
	if instanceDir != "" && strings.Contains(string(raw), instanceDir) {
		t.Fatal("the artifact leaked an instance directory path")
	}

	if _, err := runDiagnoseForTest(t, "--state", stateDir, "--db", filepath.Join(stateDir, "hub.db"), "--output", output); err == nil {
		t.Fatal("a second export must refuse to overwrite without --force")
	}
	if _, err := runDiagnoseForTest(t, "--state", stateDir, "--db", filepath.Join(stateDir, "hub.db"), "--output", output, "--force"); err != nil {
		t.Fatalf("export with --force failed: %v", err)
	}
}

func TestDiagnoseRejectsSymlinkDestination(t *testing.T) {
	stateDir := t.TempDir()
	target := filepath.Join(stateDir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(stateDir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := runDiagnoseForTest(t, "--state", stateDir, "--db", filepath.Join(stateDir, "hub.db"), "--output", link, "--force"); err == nil {
		t.Fatal("a symlink destination must be rejected even with --force")
	}
}

func TestDiagnoseRemoteGapYieldsPartialArtifact(t *testing.T) {
	stateDir := t.TempDir()
	output := filepath.Join(stateDir, "diag.json")
	if _, err := runDiagnoseForTest(t, "--state", stateDir, "--db", filepath.Join(stateDir, "hub.db"), "--output", output, "--remote"); err != errDegraded {
		t.Fatalf("--remote without hub support must be partial (exit 3), got %v", err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var data diagnoseData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range data.Findings {
		if finding.Code == "remote_diagnostics_unsupported" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the remote capability gap was not recorded: %+v", data.Findings)
	}
	if data.Status == nil {
		t.Fatal("the local status must still be exported")
	}
}

func TestDiagnoseDropsSectionsForSizeCap(t *testing.T) {
	stateDir := t.TempDir()
	id := strings.Repeat("2", 32)
	now := time.Now()
	seedLogInstance(t, stateDir, id, "ingest", now.UnixMilli(), []observe.LogEntry{
		logEntryAt(id, "ingest", observe.LevelWarn, observe.EventSourceStopped, now.Add(-time.Minute)),
	})
	data := buildDiagnose(context.Background(), diagnoseOptions{
		stateDir:   stateDir,
		setupState: stateDir,
		dbPath:     filepath.Join(stateDir, "hub.db"),
		home:       stateDir,
		nowMS:      func() int64 { return time.Now().UnixMilli() },
	}, 512)
	if len(data.Dropped) == 0 {
		t.Fatalf("no section was dropped for a 512-byte cap: %+v", data)
	}
	var kept []string
	for _, name := range data.Dropped {
		if name == "logs" {
			kept = append(kept, name)
		}
	}
	if data.Logs != nil && len(data.Dropped) >= 4 {
		t.Fatalf("logs survived every drop step: %+v", data.Dropped)
	}
}

func TestDiagnoseNeverExportsSeededSecrets(t *testing.T) {
	stateDir := t.TempDir()
	secrets := []string{
		"sk-ant-t096-transcript-secret",
		"sk-ant-t096-sql-literal-secret",
		"sk-ant-t096-annotation-secret",
		"sk-ant-t096-source-path-secret",
		"sk-ant-t096-error-secret",
		"sk-ant-t096-pairing-secret",
	}
	id := strings.Repeat("3", 32)
	now := time.Now()
	instanceDir := seedLogInstance(t, stateDir, id, "ingest", now.UnixMilli(), []observe.LogEntry{
		logEntryAt(id, "ingest", observe.LevelWarn, observe.EventCommitFailed, now.Add(-time.Minute)),
	})

	secretRoot := filepath.Join(stateDir, "projects", secrets[3]+"-dir", "projects")
	if err := os.MkdirAll(secretRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(secretRoot, "session.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","message":"`+secrets[0]+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(stateDir, "hub.db")
	dbBlob := []byte(`sqlite format 3 ... insert into events values ('` + secrets[1] +
		`') ... annotation value '` + secrets[2] + `' ... transport error: ` + secrets[4])
	if err := os.WriteFile(dbFile, dbBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "device.json"), []byte(`{"secret":"`+secrets[5]+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tornFile, err := os.OpenFile(filepath.Join(instanceDir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	torn := []byte(`{"format_version":1,"time_ms":` + jsonInt(now.Add(-time.Minute).UnixMilli()) +
		`,"error":"` + secrets[4] + `"}` + "\n")
	if _, err := tornFile.Write(torn); err != nil {
		t.Fatal(err)
	}
	tornFile.Close()

	output := filepath.Join(stateDir, "diag.json")
	if _, err := runDiagnoseForTest(t, "--state", stateDir, "--db", dbFile, "--output", output, "--force"); err != nil {
		t.Fatalf("diagnose failed: %v", err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the artifact leaked %q", secret)
		}
	}

	_, out, err := runLogsForTest(t, "--state", stateDir, "--since", "1h", "--json")
	if err != nil && err != errDegraded {
		t.Fatalf("logs failed: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Fatalf("ohya logs leaked %q", secret)
		}
	}
}

func jsonInt(v int64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
