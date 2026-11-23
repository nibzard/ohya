package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestServeArgsProblem(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "current flags pass", args: []string{"--db", "/x/hub.db", "--state", "/x", "--observe", "off"}},
		{name: "removed control-listen", args: []string{"--db", "/x/hub.db", "--control-listen", "10.0.0.1:9417"},
			want: "flag provided but not defined: -control-listen"},
		{name: "removed tls-cert", args: []string{"--tls-cert", "/t/cert.pem"},
			want: "flag provided but not defined: -tls-cert"},
		{name: "removed observe value still parsed", args: []string{"--observe", "bogus"},
			want: `--observe must be on or off, got "bogus"`},
		{name: "positional argument", args: []string{"extra"}, want: "unexpected argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := serveArgsProblem(tc.args)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("serveArgsProblem(%q) = %v; want accepted", tc.args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("serveArgsProblem(%q) = %v; want %q", tc.args, err, tc.want)
			}
		})
	}
}

func writeHubUnitFile(t *testing.T, home, name string, words ...string) string {
	t.Helper()
	var quoted []string
	for _, w := range words {
		quoted = append(quoted, `"`+strings.ReplaceAll(w, `"`, `\"`)+`"`)
	}
	content := "[Unit]\nDescription=ohya hub (serve)\nStartLimitIntervalSec=60\nStartLimitBurst=5\n\n[Service]\nType=simple\nExecStart=" +
		strings.Join(quoted, " ") + "\nWorkingDirectory=" + home + "\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n"
	path := filepath.Join(home, ".config", "systemd", "user", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, content)
	return path
}

func hubUnitDoctorHome(t *testing.T) (home, db, state string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	db = filepath.Join(home, "hub", "hub.db")
	state = filepath.Join(home, "state")
	return home, db, state
}

func stubHubUnitMainPID(t *testing.T, pid int) {
	t.Helper()
	previous := hubUnitMainPID
	hubUnitMainPID = func(context.Context, string) (int, error) { return pid, nil }
	t.Cleanup(func() { hubUnitMainPID = previous })
}

func TestDoctorHubUnitWithRemovedFlagsIsAnError(t *testing.T) {
	home, db, state := hubUnitDoctorHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeHubUnitFile(t, home, "ohya-hub.service", append([]string{self, "serve", "--db", db, "--state", state},
		"--control-listen", "10.0.0.1:9417",
		"--stun-listen", "10.0.0.1:3478",
		"--tls-cert", filepath.Join(home, "tls", "cert.pem"),
		"--tls-key", filepath.Join(home, "tls", "key.pem"),
		"--control-url", "https://hub.example:9417")...)
	stubHubUnitMainPID(t, 0)

	code, out, _ := runCLI("doctor", "--db", db, "--state", state)
	if code != 1 {
		t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "hub_unit")
	if f.Severity != sevError {
		t.Fatalf("severity = %s; want error", f.Severity)
	}
	for _, want := range []string{"ohya-hub.service", "control-listen", "cannot start again"} {
		if !strings.Contains(f.Detail, want) {
			t.Fatalf("detail misses %q: %s", want, f.Detail)
		}
	}
	for _, want := range []string{"ohya setup --hub", "ohya-serve.service", "systemctl --user disable --now ohya-hub.service"} {
		if !strings.Contains(f.Remedy, want) {
			t.Fatalf("remedy misses %q: %s", want, f.Remedy)
		}
	}
}

func TestDoctorHubUnitOwnedNameNamesSetupRepair(t *testing.T) {
	home, db, state := hubUnitDoctorHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeHubUnitFile(t, home, "ohya-serve.service", self, "serve", "--db", db, "--state", state, "--stun-listen", "10.0.0.1:3478")
	stubHubUnitMainPID(t, 0)

	code, out, _ := runCLI("doctor", "--db", db, "--state", state)
	if code != 1 {
		t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "hub_unit")
	if f.Severity != sevError {
		t.Fatalf("severity = %s; want error", f.Severity)
	}
	if !strings.Contains(f.Remedy, "rerun ohya setup --hub to rewrite ohya-serve.service") {
		t.Fatalf("remedy misses the owned-unit repair: %s", f.Remedy)
	}
	if strings.Contains(f.Remedy, "disable --now") {
		t.Fatalf("remedy tells the operator to disable the owned unit: %s", f.Remedy)
	}
}

func TestDoctorHubUnitHealthyFlagsAreOk(t *testing.T) {
	home, db, state := hubUnitDoctorHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeHubUnitFile(t, home, "ohya-serve.service", self, "serve", "--db", db, "--state", state)
	stubHubUnitMainPID(t, 0)

	code, out, _ := runCLI("doctor", "--db", db, "--state", state)
	if code != 0 {
		t.Fatalf("exit code %d; want 0 (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "hub_unit")
	if f.Severity != sevOK || !strings.Contains(f.Detail, "accepts") || !strings.Contains(f.Detail, "ohya-serve.service") {
		t.Fatalf("finding = %+v; want ok naming the unit", f)
	}
}

func TestDoctorIgnoresServeUnitsForOtherBinaries(t *testing.T) {
	home, db, state := hubUnitDoctorHome(t)
	writeHubUnitFile(t, home, "ohya-hub.service", filepath.Join(home, "other-ohya"), "serve", "--db", db, "--control-listen", "10.0.0.1:9417")
	stubHubUnitMainPID(t, 0)

	code, out, _ := runCLI("doctor", "--db", db, "--state", state)
	if code != 0 {
		t.Fatalf("exit code %d; want 0 (stdout %q)", code, out)
	}
	for _, f := range parseDoctorFindings(t, out) {
		if f.Check == "hub_unit" {
			t.Fatalf("unit for another binary produced a finding: %+v", f)
		}
	}
}

func TestDoctorHubUnitRunningReplacedExecutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the replaced-executable check reads /proc/<pid>/exe")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary to spawn")
	}
	home, db, state := hubUnitDoctorHome(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	replaced := filepath.Join(home, "ohya-replaced-bin")
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replaced, data, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(replaced, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if err := os.Remove(replaced); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "exe"))
	if err != nil || !strings.HasSuffix(link, " (deleted)") {
		t.Fatalf("fixture does not show a replaced executable: %q %v", link, err)
	}

	writeHubUnitFile(t, home, "ohya-hub.service", self, "serve", "--db", db, "--state", state)
	stubHubUnitMainPID(t, cmd.Process.Pid)

	code, out, _ := runCLI("doctor", "--db", db, "--state", state)
	if code != 1 {
		t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "hub_unit")
	if f.Severity != sevError {
		t.Fatalf("severity = %s; want error", f.Severity)
	}
	for _, want := range []string{"ohya-hub.service", strconv.Itoa(cmd.Process.Pid), "(deleted)", "not running the installed binary"} {
		if !strings.Contains(f.Detail, want) {
			t.Fatalf("detail misses %q: %s", want, f.Detail)
		}
	}
	if !strings.Contains(f.Remedy, "systemctl --user restart ohya-hub.service") {
		t.Fatalf("remedy misses the restart step: %s", f.Remedy)
	}
}

func TestDoctorExplicitRemoteJoinStateDoesNotDoubleAppendJoin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	joinDir := filepath.Join(dir, ".ohya", "join")
	if err := os.MkdirAll(filepath.Join(joinDir, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(joinDir, "setup", "connection.json"), []byte(`{"mode":"remote"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deviceID := "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(joinDir, "device.json"), []byte(`{"version":1,"device_id":"`+deviceID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(joinDir, "credential.json"), []byte(`{"version":1,"device_id":"`+deviceID+`","secret":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, out, _ := runCLI("doctor", "--state", joinDir)
	f := soleFinding(t, parseDoctorFindings(t, out), "device_identity")
	if f.Severity != sevOK || !strings.Contains(f.Detail, deviceID) {
		t.Fatalf("device_identity finding = %+v (all output:\n%s), want ok with device %s", f, out, deviceID)
	}
}

func TestCollapseSetupOKKeepsProblemsVisible(t *testing.T) {
	findings := []finding{
		{Check: "schema_version", Severity: sevOK, Detail: "ok"},
		{Check: "setup:service", Severity: sevOK, Detail: "owned integration is configured"},
		{Check: "setup:claude", Severity: sevWarn, Detail: "entry changed", Remedy: "rerun ohya setup"},
		{Check: "setup:codex", Severity: sevOK, Detail: "owned integration is configured"},
	}
	got := collapseSetupOK(findings)
	if len(got) != 3 {
		t.Fatalf("collapsed = %+v; want 3 findings", got)
	}
	if got[1].Check != "setup" || got[1].Detail != "2 owned integrations configured: service, codex" {
		t.Fatalf("collapsed setup finding = %+v", got[1])
	}
	if got[2].Check != "setup:claude" || got[2].Severity != sevWarn {
		t.Fatalf("warning finding lost: %+v", got[2])
	}
}
