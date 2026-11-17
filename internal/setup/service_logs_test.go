package setup

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func plistStrings(t *testing.T, content []byte) map[string][]string {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(string(content)))
	var key string
	out := map[string][]string{}
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode plist: %v", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Local == "key" {
				var text string
				if err := d.DecodeElement(&text, &element); err != nil {
					t.Fatalf("decode key: %v", err)
				}
				key = text
			} else if element.Name.Local == "string" || element.Name.Local == "true" || element.Name.Local == "false" || element.Name.Local == "integer" {
				var text string
				if err := d.DecodeElement(&text, &element); err != nil && element.Name.Local == "string" {
					t.Fatalf("decode value: %v", err)
				}
				switch element.Name.Local {
				case "true":
					text = "true"
				case "false":
					text = "false"
				}
				out[key] = append(out[key], text)
			}
		}
	}
	return out
}

func TestDarwinServicePlanOwnsStderrStartupFileWithoutStdoutFile(t *testing.T) {
	home := "/home/a b"
	state := "/home/a b/.ohya"
	s, err := ServicePlan("darwin", home, state, "/home/a b/ohya", []string{"ingest", "--state", state})
	if err != nil {
		t.Fatal(err)
	}
	values := plistStrings(t, s.Content)
	stderr, ok := values["StandardErrorPath"]
	if !ok || len(stderr) != 1 || stderr[0] != filepath.Join(state, "logs", "com.nibzard.ohya.stderr.log") {
		t.Fatalf("StandardErrorPath = %v", stderr)
	}
	if s.StderrPath != stderr[0] {
		t.Fatalf("plan stderr path = %q, want %q", s.StderrPath, stderr[0])
	}
	if _, hasStdout := values["StandardOutPath"]; hasStdout {
		t.Fatal("the plan must not configure an unbounded stdout file")
	}
	if keep, ok := values["KeepAlive"]; !ok || len(keep) != 1 || keep[0] != "true" {
		t.Fatalf("KeepAlive = %v; the restart policy must stay unchanged", keep)
	}
	if throttle, ok := values["ThrottleInterval"]; !ok || len(throttle) != 1 || throttle[0] != "5" {
		t.Fatalf("ThrottleInterval = %v; the restart policy must stay unchanged", throttle)
	}
	hub, err := HubServicePlan("darwin", home, state, "/home/a b/ohya", []string{"serve", "--state", state})
	if err != nil {
		t.Fatal(err)
	}
	if got := plistStrings(t, hub.Content)["StandardErrorPath"]; len(got) != 1 || got[0] != filepath.Join(state, "logs", "com.nibzard.ohya.serve.stderr.log") {
		t.Fatalf("hub StandardErrorPath = %v", got)
	}
	linux, err := ServicePlan("linux", home, state, "/home/a b/ohya", nil)
	if err != nil {
		t.Fatal(err)
	}
	if linux.StderrPath != "" || strings.Contains(string(linux.Content), "StandardErrorPath") {
		t.Fatal("the linux unit must keep the journal, not a log file")
	}
}

func TestPrepareStderrLogCreatesPrivateDirAndRotatesOversizeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "ohya.stderr.log")
	if err := PrepareStderrLog(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("log dir mode = %o, want 0700", info.Mode().Perm())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("prepare must not create the log file itself: %v", err)
	}

	if err := os.WriteFile(path, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareStderrLog(path); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "small" {
		t.Fatalf("a small log file must not rotate: %q %v", raw, err)
	}

	big := make([]byte, StderrLogMaxBytes+1)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareStderrLog(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the oversize log file must rotate away")
	}
	if raw, err := os.ReadFile(path + ".1"); err != nil || len(raw) != len(big) {
		t.Fatalf("rotation did not keep the oversize file as .1: %v", err)
	}

	if err := os.Symlink(filepath.Join(dir, "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if err := PrepareStderrLog(path); err == nil {
		t.Fatal("a symlink log path must be rejected")
	}
}

func seedObserveInstance(t *testing.T, stateDir, id string, pid int, age time.Duration) string {
	t.Helper()
	instanceDir := filepath.Join(stateDir, "observe", id)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot := `{"format_version":1,"instance":{"instance_id":"` + id + `","pid":` + strconv.Itoa(pid) +
		`,"roles":["ingest"],"start_wall_ms":1},"uptime_ms":1,"sampled_wall_ms":` + strconv.FormatInt(time.Now().Add(-age).UnixMilli(), 10) + `,"counters":{},"gauges":{},"durations":{},"resources":{}}`
	if err := os.WriteFile(filepath.Join(instanceDir, "snapshot.json"), []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	return instanceDir
}

func TestConfigureServicePreparesStderrLogAndPrunesStaleTelemetry(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	o.NoAgents = true
	o.Platform = "darwin"
	service, err := ServicePlan("darwin", o.Clients.Home, o.Clients.State, o.Clients.Binary, o.ServiceArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(service.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := seedObserveInstance(t, o.Clients.State, strings.Repeat("9", 32), 999999, 8*24*time.Hour)
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	live := seedObserveInstance(t, o.Clients.State, strings.Repeat("8", 32), os.Getpid(), time.Minute)
	o.LoadedService = func(context.Context, []string) error { return errors.New("not loaded") }
	o.RunService = func(context.Context, []string) error { return nil }
	results := configureService(context.Background(), o, service)
	if len(results) == 0 || results[len(results)-1].Status != "running" {
		t.Fatalf("configure results = %+v", results)
	}
	if _, err := os.Stat(filepath.Dir(service.StderrPath)); err != nil {
		t.Fatalf("stderr log directory was not prepared: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a stale terminated instance was not pruned")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("a live instance was pruned; retention must never delete a live instance")
	}
}

func TestUninstallPreservesDiagnosticFiles(t *testing.T) {
	o := engineOptions(t)
	service, err := ServicePlan("linux", o.Clients.Home, o.Clients.State, o.Clients.Binary, o.ServiceArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(service.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(o.Clients.State, "setup", "service.json")
	if _, err := ApplyFile(service.Path, receipt, service.Content); err != nil {
		t.Fatal(err)
	}
	instanceDir := seedObserveInstance(t, o.Clients.State, strings.Repeat("7", 32), os.Getpid(), time.Minute)
	stderrLog := filepath.Join(o.Clients.State, "logs", "ohya.stderr.log")
	if err := os.MkdirAll(filepath.Dir(stderrLog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrLog, []byte("startup failure diagnostics"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Remove(context.Background(), o.Clients.State, func(context.Context, []string) error { return nil })
	if err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}
	for _, result := range report.Results {
		if result.Status == "failed" {
			t.Fatalf("uninstall result failed: %+v", result)
		}
	}
	if _, err := os.Stat(instanceDir); err != nil {
		t.Fatal("uninstall deleted published telemetry; diagnostic files must be preserved")
	}
	if raw, err := os.ReadFile(stderrLog); err != nil || len(raw) == 0 {
		t.Fatal("uninstall deleted the service stderr startup file; diagnostic files must be preserved")
	}
}
