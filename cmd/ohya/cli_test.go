package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/registry"
)

func helpCases() []struct {
	name string
	args []string
	spec commandSpec
} {
	return []struct {
		name string
		args []string
		spec commandSpec
	}{
		{"ohya", nil, rootSpec},
		{"ohya --help", []string{"--help"}, rootSpec},
		{"ohya -h", []string{"-h"}, rootSpec},
		{"ingest --help", []string{"ingest", "--help"}, ingestSpec},
		{"serve --help", []string{"serve", "--help"}, serveSpec},
		{"join --help", []string{"join", "--help"}, joinSpec},
		{"mcp --help", []string{"mcp", "--help"}, mcpSpec},
		{"search --help", []string{"search", "--help"}, searchSpec},
		{"show --help", []string{"show", "--help"}, showSpec},
		{"sessions --help", []string{"sessions", "--help"}, sessionsSpec},
		{"rederive --help", []string{"rederive", "--help"}, rederiveSpec},
		{"doctor --help", []string{"doctor", "--help"}, doctorSpec},
		{"version --help", []string{"version", "--help"}, versionSpec},
		{"devices", []string{"devices"}, devicesSpec},
		{"devices --help", []string{"devices", "--help"}, devicesSpec},
		{"devices -h", []string{"devices", "-h"}, devicesSpec},
		{"devices invite --help", []string{"devices", "invite", "--help"}, devicesInviteSpec},
		{"devices list --help", []string{"devices", "list", "--help"}, devicesListSpec},
		{"devices revoke --help", []string{"devices", "revoke", "--help"}, devicesRevokeSpec},
		{"service", []string{"service"}, serviceSpec},
		{"service --help", []string{"service", "--help"}, serviceSpec},
		{"service status --help", []string{"service", "status", "--help"}, serviceStatusSpec},
		{"service start --help", []string{"service", "start", "--help"}, serviceStartSpec},
		{"service stop --help", []string{"service", "stop", "--help"}, serviceStopSpec},
		{"service restart --help", []string{"service", "restart", "--help"}, serviceRestartSpec},
	}
}

func TestHelpIsSuccessOnStdout(t *testing.T) {
	for _, tc := range helpCases() {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := cliMain(tc.args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d; want 0 (stderr: %q)", code, stderr.String())
			}
			if stderr.Len() > 0 {
				t.Fatalf("stderr = %q; want empty", stderr.String())
			}
			out := stdout.String()
			for _, section := range []string{
				"Usage:",
				tc.spec.argsTitle + ":",
				"Flags:",
				"Examples:",
			} {
				if !strings.Contains(out, section) {
					t.Fatalf("help output lacks %q:\n%s", section, out)
				}
			}
			if !strings.Contains(out, "  "+tc.spec.usageLine+"\n") {
				t.Fatalf("help output lacks the usage line %q:\n%s", tc.spec.usageLine, out)
			}
			if !strings.Contains(out, "--help") {
				t.Fatalf("help output lacks the --help flag:\n%s", out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "  -") && !strings.HasPrefix(line, "  --") {
					t.Fatalf("flag spelled with one dash: %q", line)
				}
			}
		})
	}
}

func TestRootHelpStatesContract(t *testing.T) {
	var stdout bytes.Buffer
	if code := cliMain(nil, &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"non-interactive by design",
		"0 success, 1 failure, 2 usage error, 3 degraded",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("root help lacks %q:\n%s", want, out)
		}
	}
}

func TestSearchHelpStatesFtsQuotingRule(t *testing.T) {
	var stdout bytes.Buffer
	if code := cliMain([]string{"search", "--help"}, &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}
	flat := strings.Join(strings.Fields(stdout.String()), " ")
	for _, want := range []string{
		"A term with punctuation",
		"must sit inside double quotes",
		`ohya search '"hyphenated-name"'`,
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("search help lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestUsageErrorsExitTwoOnStderr(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantMsg string
		wantCmd string
	}{
		{"unknown subcommand", []string{"frobnicate"}, `unknown subcommand "frobnicate"`, "ohya"},
		{"unknown devices subcommand", []string{"devices", "zap"}, `unknown devices subcommand "zap"`, "ohya devices"},
		{"bad flag", []string{"ingest", "--nope"}, "flag provided but not defined: -nope", "ohya ingest"},
		{"bad devices flag", []string{"devices", "list", "--nope"}, "flag provided but not defined: -nope", "ohya devices list"},
		{"bad flag value", []string{"devices", "invite", "--ttl", "soon"}, "invalid value", "ohya devices invite"},
		{"missing flag value", []string{"ingest", "--db"}, "flag needs an argument", "ohya ingest"},
		{"missing flag value after positional", []string{"join", "CODE", "--state"}, "flag needs an argument", "ohya join"},
		{"unknown flag after positional", []string{"devices", "revoke", "ID", "--nope"}, "flag provided but not defined: -nope", "ohya devices revoke"},
		{"missing argument", []string{"devices", "revoke"}, "revoke needs exactly one device ID", "ohya devices revoke"},
		{"search without query", []string{"search"}, "search needs exactly one FTS query argument", "ohya search"},
		{"search with two queries", []string{"search", "a", "b"}, "search needs exactly one FTS query argument", "ohya search"},
		{"bad search flag", []string{"search", "--nope", "x"}, "flag provided but not defined: -nope", "ohya search"},
		{"show without session id", []string{"show"}, "show needs exactly one session id", "ohya show"},
		{"show last zero", []string{"show", "--last", "0", "s"}, "--last must be a positive integer", "ohya show"},
		{"show last not a number", []string{"show", "--last", "soon", "s"}, "invalid value", "ohya show"},
		{"two device IDs", []string{"devices", "revoke", "a", "--state", "s", "b"}, "revoke needs exactly one device ID", "ohya devices revoke"},
		{"unexpected argument", []string{"rederive", "extra"}, `unexpected argument "extra"`, "ohya rederive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := cliMain(tc.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d; want 2", code)
			}
			if stdout.Len() > 0 {
				t.Fatalf("stdout = %q; want empty", stdout.String())
			}
			line := stderr.String()
			if got := strings.Count(line, "\n"); got != 1 {
				t.Fatalf("stderr = %q; want exactly one line", line)
			}
			if !strings.Contains(line, tc.wantMsg) {
				t.Fatalf("stderr = %q; want it to contain %q", line, tc.wantMsg)
			}
			if !strings.Contains(line, "'"+tc.wantCmd+" --help'") {
				t.Fatalf("stderr = %q; want a pointer to %s --help", line, tc.wantCmd)
			}
		})
	}
}

func TestProcessExitCodes(t *testing.T) {
	blocker, err := os.CreateTemp(t.TempDir(), "blocker")
	if err != nil {
		t.Fatal(err)
	}
	badDB := filepath.Join(blocker.Name(), "hub.db")

	cases := []struct {
		name       string
		args       []string
		code       int
		wantStdout bool
		wantStderr bool
	}{
		{"bare command", nil, 0, true, false},
		{"long help", []string{"--help"}, 0, true, false},
		{"subcommand help", []string{"mcp", "--help"}, 0, true, false},
		{"devices subcommand help", []string{"devices", "revoke", "--help"}, 0, true, false},
		{"unknown subcommand", []string{"frobnicate"}, 2, false, true},
		{"bad flag", []string{"ingest", "--nope"}, 2, false, true},
		{"failure", []string{"mcp", "--db", badDB}, 1, false, true},
		{"version", []string{"version"}, 0, true, false},
		{"long version", []string{"--version"}, 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], append([]string{"child-run"}, tc.args...)...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			runErr := cmd.Run()
			code := 0
			if runErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) {
					t.Fatal(runErr)
				}
				code = exitErr.ExitCode()
			}
			if code != tc.code {
				t.Fatalf("exit code = %d; want %d (stderr: %q)", code, tc.code, stderr.String())
			}
			if (stdout.Len() > 0) != tc.wantStdout {
				t.Fatalf("stdout = %q; want output: %t", stdout.String(), tc.wantStdout)
			}
			if (stderr.Len() > 0) != tc.wantStderr {
				t.Fatalf("stderr = %q; want output: %t", stderr.String(), tc.wantStderr)
			}
		})
	}
}

func TestPermuteFlags(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		want     []string
		wantBare string
	}{
		{"no flags", []string{"a", "b"}, []string{"a", "b"}, ""},
		{"flags first", []string{"--state", "d", "a"}, []string{"--state", "d", "a"}, ""},
		{"flags after positional", []string{"a", "--state", "d"}, []string{"--state", "d", "a"}, ""},
		{"flags between positionals", []string{"a", "--json", "b"}, []string{"--json", "a", "b"}, ""},
		{"equals form", []string{"a", "--state=d"}, []string{"--state=d", "a"}, ""},
		{"duration value", []string{"a", "--interval", "5s"}, []string{"--interval", "5s", "a"}, ""},
		{"value taken from flag-looking argument", []string{"a", "--state", "--json"}, []string{"--state", "--json", "a"}, ""},
		{"terminator keeps rest positional", []string{"--state", "d", "--", "a", "--json"}, []string{"--state", "d", "--", "a", "--json"}, ""},
		{"terminator after positional", []string{"a", "--", "--json"}, []string{"a", "--", "--json"}, ""},
		{"unknown flag stays a flag", []string{"a", "--nope", "b"}, []string{"--nope", "a", "b"}, ""},
		{"trailing flag without value", []string{"a", "--state"}, nil, "--state"},
		{"single dash form", []string{"a", "-state", "d"}, []string{"-state", "d", "a"}, ""},
		{"single dash alone is positional", []string{"-", "--json"}, []string{"--json", "-"}, ""},
		{"no arguments", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("state", "", "")
			fs.Bool("json", false, "")
			fs.Duration("interval", 0, "")
			got, bare := permuteFlags(fs, tc.in)
			if !reflect.DeepEqual(got, tc.want) || bare != tc.wantBare {
				t.Fatalf("permuteFlags(%q) = %q, %q; want %q, %q", tc.in, got, bare, tc.want, tc.wantBare)
			}
		})
	}
}

func TestJoinParsesFlagsOnEitherSideOfPositional(t *testing.T) {
	cases := []struct {
		name           string
		flags          func(state string) []string
		code           int
		wantErr        string
		wantDeviceFile bool
	}{
		{"state applied", func(state string) []string { return []string{"--state", state} },
			1, "a join code starts with ohya1.", true},
		{"interval applied", func(state string) []string {
			return []string{"--state", state, "--interval", "0"}
		}, 2, "--interval must be positive", false},
	}
	for _, tc := range cases {
		for _, side := range []string{"before", "after"} {
			t.Run(tc.name+" — flags "+side, func(t *testing.T) {
				state := t.TempDir()
				args := []string{"join"}
				if side == "before" {
					args = append(append(args, tc.flags(state)...), "notacode")
				} else {
					args = append(append(args, "notacode"), tc.flags(state)...)
				}
				var stdout, stderr bytes.Buffer
				if got := cliMain(args, &stdout, &stderr); got != tc.code {
					t.Fatalf("exit code = %d; want %d (stderr: %q)", got, tc.code, stderr.String())
				}
				if !strings.Contains(stderr.String(), tc.wantErr) {
					t.Fatalf("stderr = %q; want it to contain %q", stderr.String(), tc.wantErr)
				}
				if tc.wantDeviceFile {
					if _, err := os.Stat(filepath.Join(state, "device.json")); err != nil {
						t.Fatalf("the join state directory holds no device identity: %v", err)
					}
				}
			})
		}
	}
}

func TestDevicesRevokeParsesFlagsOnEitherSideOfPositional(t *testing.T) {
	deviceID := "0123456789abcdef0123456789abcdef"
	cases := []struct {
		side string
		json bool
	}{
		{"before", false},
		{"after", false},
		{"before", true},
		{"after", true},
	}
	for _, tc := range cases {
		name := "flags " + tc.side
		if tc.json {
			name += " with --json"
		}
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			reg, err := registry.Open(filepath.Join(state, registry.FileName))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			inv, err := reg.Issue(ctx, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reg.Consume(ctx, inv.Secret, deviceID); err != nil {
				t.Fatal(err)
			}
			if err := reg.Close(); err != nil {
				t.Fatal(err)
			}

			flags := []string{"--state", state}
			if tc.json {
				flags = append(flags, "--json")
			}
			args := []string{"revoke"}
			if tc.side == "before" {
				args = append(append(args, flags...), deviceID)
			} else {
				args = append(append(args, deviceID), flags...)
			}
			var stdout, stderr bytes.Buffer
			if err := runDevicesCommand(args, &stdout, &stderr); err != nil {
				t.Fatalf("revoke err = %v (stderr: %q)", err, stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, deviceID) {
				t.Fatalf("stdout = %q; want the revoked device ID", out)
			}
			if tc.json {
				if !strings.Contains(out, `"device_id":"`+deviceID+`"`) {
					t.Fatalf("stdout = %q; want the JSON device_id", out)
				}
			} else if !strings.Contains(out, "revoked") {
				t.Fatalf("stdout = %q; want the revoked confirmation", out)
			}
		})
	}
}
