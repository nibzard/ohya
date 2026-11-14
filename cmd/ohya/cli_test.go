package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		{"rederive --help", []string{"rederive", "--help"}, rederiveSpec},
		{"doctor --help", []string{"doctor", "--help"}, doctorSpec},
		{"version --help", []string{"version", "--help"}, versionSpec},
		{"devices", []string{"devices"}, devicesSpec},
		{"devices --help", []string{"devices", "--help"}, devicesSpec},
		{"devices -h", []string{"devices", "-h"}, devicesSpec},
		{"devices invite --help", []string{"devices", "invite", "--help"}, devicesInviteSpec},
		{"devices list --help", []string{"devices", "list", "--help"}, devicesListSpec},
		{"devices revoke --help", []string{"devices", "revoke", "--help"}, devicesRevokeSpec},
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
		{"missing argument", []string{"devices", "revoke"}, "revoke needs exactly one device ID", "ohya devices revoke"},
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
