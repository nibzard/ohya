package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/watch"
)

func TestJoinCodeEnvValidation(t *testing.T) {
	t.Setenv("OHYA_EMPTY_JOIN_CODE", "")
	t.Setenv("OHYA_BAD_JOIN_CODE", "not-a-valid-code")
	t.Setenv("OHYA_GOOD_JOIN_CODE", "ohya1.synthetic-token.tcaddr")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"both", []string{"join", "ohya1.a.b", "--join-code-env", "OHYA_GOOD_JOIN_CODE"}, "not both"},
		{"bad name", []string{"join", "--join-code-env", "1BAD"}, "invalid join code environment variable name"},
		{"empty", []string{"join", "--join-code-env", "OHYA_EMPTY_JOIN_CODE"}, "join code environment variable is empty"},
		{"invalid code", []string{"join", "--join-code-env", "OHYA_BAD_JOIN_CODE"}, "join code environment variable does not hold one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI(tc.args...)
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("args %v -> code %d stderr %q, want 2 with %q", tc.args, code, stderr, tc.want)
			}
		})
	}
}

func TestJoinContextDeadlineDuringStartupFails(t *testing.T) {
	stateDir := t.TempDir()
	code := formatJoinCode("0123456789abcdef0123456789abcdef", unpairedHubAddress(t))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var stderr bytes.Buffer
	err := runJoin(ctx, joinOptions{pairOnly: true, stateDir: stateDir, code: code, stderr: &stderr})
	if err == nil {
		t.Fatal("an expired pairing deadline is a failure, not a clean stop; only SIGINT or SIGTERM stops join cleanly")
	}
	if _, err := os.Stat(filepath.Join(stateDir, device.CredentialFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a credential was paired despite the expired deadline: %v", err)
	}
}

func TestReportJoinBacklogClampsTruncatedFile(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	srcDir := filepath.Join(tmp, "claude")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(srcDir, "sess.jsonl")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 200), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := watch.New(stateDir, "claude-code", watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := w.Poll([]string{path})
	if err != nil || len(outcomes) != 1 {
		t.Fatalf("Poll: %v (%d)", err, len(outcomes))
	}
	if err := w.Acknowledge(path, outcomes[0].SourceID, 200); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	reportJoinBacklog(stateDir, []agentRoot{{agent: "claude-code", dirs: []agentDir{{dir: srcDir}}}}, nil, &stderr)
	if !strings.Contains(stderr.String(), "upload backlog: 0 bytes") {
		t.Fatalf("reportJoinBacklog = %q, want 0 bytes", stderr.String())
	}
}

func TestReportJoinBacklogResolvesSymlinkedRoots(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	realDir := filepath.Join(tmp, "real", "claude")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(tmp, "link-root")
	if err := os.Symlink(filepath.Join(tmp, "real"), linkRoot); err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(linkRoot, "claude")
	truncated := filepath.Join(srcDir, "sess.jsonl")
	if err := os.WriteFile(truncated, bytes.Repeat([]byte("a"), 200), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := watch.New(stateDir, "claude-code", watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := w.Poll([]string{truncated})
	if err != nil || len(outcomes) != 1 {
		t.Fatalf("Poll: %v (%d)", err, len(outcomes))
	}
	if err := w.Acknowledge(truncated, outcomes[0].SourceID, 200); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(truncated, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(srcDir, "fresh.jsonl")
	if err := os.WriteFile(fresh, bytes.Repeat([]byte("b"), 50), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	reportJoinBacklog(stateDir, []agentRoot{{agent: "claude-code", dirs: []agentDir{{dir: srcDir}}}}, nil, &stderr)
	if !strings.Contains(stderr.String(), "upload backlog: 50 bytes") {
		t.Fatalf("reportJoinBacklog = %q, want only the fresh file's 50 bytes; an acknowledged source behind a symlinked root must not count as backlog", stderr.String())
	}
}

func TestReportJoinBacklogSkipsExcludedFiles(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	srcDir := filepath.Join(tmp, "claude")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(srcDir, "kept.jsonl")
	excluded := filepath.Join(srcDir, "guardian.jsonl")
	if err := os.WriteFile(kept, bytes.Repeat([]byte("a"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excluded, bytes.Repeat([]byte("b"), 200), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	reportJoinBacklog(stateDir, []agentRoot{{agent: "claude-code", dirs: []agentDir{{dir: srcDir}}}}, excludeGlobs{excluded}, &stderr)
	if !strings.Contains(stderr.String(), "upload backlog: 100 bytes") {
		t.Fatalf("reportJoinBacklog = %q, want only the kept file's 100 bytes", stderr.String())
	}
}

func unpairedHubAddress(t *testing.T) string {
	t.Helper()
	ci := tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: key.NewNode().Public()},
		ServerDiscoPublic: tailcat.DiscoPublic{DiscoPublic: key.NewDisco().Public()},
		RegionID:          -1,
	}
	return string(ci.Addr())
}

func TestJoinContextCancelDuringStartupStopsCleanly(t *testing.T) {
	cases := []struct {
		name       string
		paired     bool
		passCode   bool
		wantCredOK bool
	}{
		{name: "pair window", passCode: true},
		{name: "rejoin window", paired: true, passCode: true, wantCredOK: true},
		{name: "resume window", paired: true, wantCredOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			code := ""
			if tc.passCode {
				code = formatJoinCode("0123456789abcdef0123456789abcdef", unpairedHubAddress(t))
			}
			if tc.paired {
				deviceID, err := device.LoadOrCreate(stateDir)
				if err != nil {
					t.Fatal(err)
				}
				cred := device.Credential{DeviceID: deviceID, Secret: "0123456789abcdef0123456789abcdef"}
				if err := device.SaveCredential(stateDir, cred); err != nil {
					t.Fatal(err)
				}
				if err := device.SaveHubAddress(stateDir, unpairedHubAddress(t)); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stderr bytes.Buffer
			err := runJoin(ctx, joinOptions{pairOnly: true, stateDir: stateDir, code: code, stderr: &stderr})
			if err != nil {
				t.Fatalf("a context canceled during join startup is a clean stop, got error: %v", err)
			}
			if out := stderr.String(); strings.Contains(out, "context canceled") {
				t.Fatalf("cancellation surfaced as a join diagnostic: %s", out)
			}
			_, credErr := os.Stat(filepath.Join(stateDir, device.CredentialFileName))
			if tc.wantCredOK {
				if credErr != nil {
					t.Fatalf("the saved credential survived the canceled stop, want it kept: %v", credErr)
				}
			} else if !errors.Is(credErr, os.ErrNotExist) {
				t.Fatalf("a credential was paired despite the canceled stop: %v", credErr)
			}
		})
	}
}

func TestRejoinWithoutHubAddressNamesTheInviteAndInstallerSteps(t *testing.T) {
	stateDir := t.TempDir()
	cred := device.Credential{DeviceID: "a4ca22cc3f546eeffd469f74abade288", Secret: "0123456789abcdef0123456789abcdef"}
	_, err := rejoin(context.Background(), nil, stateDir, cred, "", "", io.Discard)
	if err == nil {
		t.Fatal("rejoin without a code or a saved hub address succeeded")
	}
	for _, want := range []string{
		cred.DeviceID,
		"ohya devices invite",
		"install.sh | sh -s -- --join",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("rejoin error misses %q:\n%v", want, err)
		}
	}
}
