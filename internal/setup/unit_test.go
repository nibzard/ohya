package setup

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSystemdWords(t *testing.T) {
	cases := []struct {
		line string
		want []string
		fail bool
	}{
		{line: `/bin/ohya serve --db /x/hub.db`, want: []string{"/bin/ohya", "serve", "--db", "/x/hub.db"}},
		{line: `"/path with space/ohya" "serve" "--db" "/x y/hub.db"`, want: []string{"/path with space/ohya", "serve", "--db", "/x y/hub.db"}},
		{line: `'/single quoted' 'a b'`, want: []string{"/single quoted", "a b"}},
		{line: `back\ slash esc\"quote`, want: []string{"back slash", `esc"quote`}},
		{line: `  spaced   out  `, want: []string{"spaced", "out"}},
		{line: `"unterminated`, fail: true},
		{line: `'unterminated`, fail: true},
		{line: `trailing\`, fail: true},
	}
	for _, tc := range cases {
		got, err := systemdWords(tc.line)
		if tc.fail {
			if err == nil {
				t.Fatalf("systemdWords(%q) = %v; want error", tc.line, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("systemdWords(%q): %v", tc.line, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("systemdWords(%q) = %q; want %q", tc.line, got, tc.want)
		}
	}
}

func TestParseUnitExecStart(t *testing.T) {
	unit := func(execLine string) string {
		return "[Unit]\nDescription=ohya hub (serve)\nStartLimitBurst=5\n\n[Service]\nType=simple\n" +
			execLine + "\nWorkingDirectory=/home/a\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n"
	}
	removed := `ExecStart="/home/agent/.local/bin/ohya" "serve" "--db" "/home/agent/.ohya/hub.db" "--state" "/home/agent/.ohya" "--control-listen" "10.0.0.1:9417" "--stun-listen" "10.0.0.1:3478" "--tls-cert" "/t/cert.pem" "--tls-key" "/t/key.pem" "--control-url" "https://hub.example:9417"`
	exec, args, err := ParseUnitExecStart([]byte(unit(removed)))
	if err != nil {
		t.Fatal(err)
	}
	if exec != "/home/agent/.local/bin/ohya" {
		t.Fatalf("exec = %q", exec)
	}
	want := []string{"serve", "--db", "/home/agent/.ohya/hub.db", "--state", "/home/agent/.ohya",
		"--control-listen", "10.0.0.1:9417", "--stun-listen", "10.0.0.1:3478",
		"--tls-cert", "/t/cert.pem", "--tls-key", "/t/key.pem", "--control-url", "https://hub.example:9417"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q; want %q", args, want)
	}

	exec, args, err = ParseUnitExecStart([]byte(unit("ExecStart=-/bin/ohya serve")))
	if err != nil || exec != "/bin/ohya" || !reflect.DeepEqual(args, []string{"serve"}) {
		t.Fatalf("ignore-failure prefix: %q %q %v", exec, args, err)
	}

	exec, args, err = ParseUnitExecStart([]byte(unit(`ExecStart=/bin/oh%%ya $$x`)))
	if err != nil || exec != "/bin/oh%ya" || !reflect.DeepEqual(args, []string{"$x"}) {
		t.Fatalf("expansion unescape: %q %q %v", exec, args, err)
	}

	if _, _, err = ParseUnitExecStart([]byte("[Unit]\nDescription=x\n")); !errors.Is(err, ErrNoExecStart) {
		t.Fatalf("missing ExecStart err = %v", err)
	}
	if _, _, err = ParseUnitExecStart([]byte("[Service]\nExecStartPre=/bin/true\n")); !errors.Is(err, ErrNoExecStart) {
		t.Fatalf("ExecStartPre must not count: %v", err)
	}
	if _, _, err = ParseUnitExecStart([]byte("[Service]\nExecStart=\"broken\n")); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestParseUnitExecStartRoundTripsServicePlan(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ohya bin")
	if err := os.WriteFile(binary, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := ServicePlan("linux", dir, dir, binary, []string{"ingest", "--state", dir, "--codex-root", ""})
	if err != nil {
		t.Fatal(err)
	}
	exec, args, err := ParseUnitExecStart(s.Content)
	if err != nil {
		t.Fatal(err)
	}
	if exec != binary {
		t.Fatalf("exec = %q; want %q", exec, binary)
	}
	want := []string{"ingest", "--state", dir, "--codex-root", ""}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q; want %q", args, want)
	}
}

func TestScanServeUnits(t *testing.T) {
	dir := t.TempDir()
	unitDir := filepath.Join(dir, "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeUnit := func(name, execLine string) {
		t.Helper()
		content := "[Unit]\nDescription=x\n\n[Service]\nExecStart=" + execLine + "\n\n[Install]\nWantedBy=default.target\n"
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	quote := func(words ...string) string {
		var quoted []string
		for _, w := range words {
			quoted = append(quoted, `"`+strings.ReplaceAll(w, `"`, `\"`)+`"`)
		}
		return strings.Join(quoted, " ")
	}
	writeUnit("ohya-hub.service", quote(self, "serve", "--db", filepath.Join(dir, "hub.db")))
	writeUnit("other-bin.service", quote(filepath.Join(dir, "other-ohya"), "serve"))
	writeUnit("ohya-ingest.service", quote(self, "ingest", "--once"))
	writeUnit("not-a-service.txt", quote(self, "serve"))

	units, err := ScanServeUnits(unitDir, self)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 {
		t.Fatalf("units = %+v; want only the matching serve unit", units)
	}
	u := units[0]
	if u.Unit != "ohya-hub.service" || u.Exec != self || !reflect.DeepEqual(u.Args, []string{"serve", "--db", filepath.Join(dir, "hub.db")}) {
		t.Fatalf("unit = %+v", u)
	}

	if units, err = ScanServeUnits(filepath.Join(dir, "absent"), self); err != nil || units != nil {
		t.Fatalf("absent dir: %+v %v", units, err)
	}
}

func TestParseMainPID(t *testing.T) {
	cases := []struct {
		out  string
		want int
		fail bool
	}{
		{out: "45514\n", want: 45514},
		{out: "0", want: 0},
		{out: " 12 \n", want: 12},
		{out: "", fail: true},
		{out: "junk\n", fail: true},
	}
	for _, tc := range cases {
		got, err := parseMainPID(tc.out)
		if tc.fail {
			if err == nil {
				t.Fatalf("parseMainPID(%q) = %d; want error", tc.out, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("parseMainPID(%q) = %d, %v; want %d", tc.out, got, err, tc.want)
		}
	}
}
