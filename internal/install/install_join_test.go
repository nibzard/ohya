package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func startFixtureRelease(t *testing.T) (root, caFile string) {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	tw := tar.NewWriter(compressed)
	for _, name := range []string{"INSTALL.md", "LICENSE", "ohya"} {
		data := []byte("fixture")
		if name == "ohya" {
			data = []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo 'ohya version=v9.8.7'; else printf '%s\\n' \"$@\"; fi\n")
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0700, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	asset := fmt.Sprintf("ohya_v9.8.7_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			http.Redirect(w, r, "/tag/v9.8.7", http.StatusFound)
		case "/tag/v9.8.7":
			_, _ = w.Write([]byte("fixture release"))
		case "/download/v9.8.7/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", sum, asset)
		case "/download/v9.8.7/" + asset:
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return server.URL, ca
}

func runInstaller(ctx context.Context, t *testing.T, script, ca string, stdin *strings.Reader, env []string, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, "sh", append([]string{script}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(append(os.Environ(), env...), "CURL_CA_BUNDLE="+ca, "TMPDIR="+dir)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	output, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		t.Fatalf("installer timed out; output:\n%s", output)
	}
	return string(output), err
}

func TestInstallerJoinWithoutTerminalRequiresEnv(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("shell bootstrap targets Unix")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	root, ca := startFixtureRelease(t)
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	output, err := runInstaller(ctx, t, script, ca, nil,
		[]string{"OHYA_RELEASE_ROOT=" + root}, "--join")
	if err == nil {
		t.Fatalf("accepted --join without a terminal: %s", output)
	}
	if !strings.Contains(output, "--join-code-env") {
		t.Fatalf("output = %s; want the --join-code-env requirement", output)
	}
}

func TestInstallerJoinWithJSONPromptsNothing(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("shell bootstrap targets Unix")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	root, ca := startFixtureRelease(t)
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	output, err := runInstaller(ctx, t, script, ca, nil,
		[]string{"OHYA_RELEASE_ROOT=" + root}, "--join", "--json")
	if err == nil {
		t.Fatalf("accepted --join --json without a code: %s", output)
	}
	if !strings.Contains(output, "--join-code-env") {
		t.Fatalf("output = %s; want the --join-code-env requirement", output)
	}
	if strings.Contains(output, "join code:") {
		t.Fatalf("output = %s; --json must not prompt", output)
	}
}

func TestInstallerJoinCodeEnvPassesThrough(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("shell bootstrap targets Unix")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	root, ca := startFixtureRelease(t)
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	output, err := runInstaller(ctx, t, script, ca, nil,
		[]string{"OHYA_RELEASE_ROOT=" + root, "OHYA_X=ohya1.invitation.tcfixture"},
		"--join-code-env", "OHYA_X", "--no-agents", "--no-service")
	if err != nil {
		t.Fatalf("installer: %v; %s", err, output)
	}
	if !strings.Contains(output, "--join-code-env\n") || !strings.Contains(output, "OHYA_X\n") {
		t.Fatalf("output = %s; want the env flag passthrough", output)
	}
	if strings.Contains(output, "ohya1.invitation.tcfixture") {
		t.Fatalf("output = %s; the code value must not appear", output)
	}

	joined, err := runInstaller(ctx, t, script, ca, nil,
		[]string{"OHYA_RELEASE_ROOT=" + root, "OHYA_X=ohya1.invitation.tcfixture"},
		"--join", "--join-code-env", "OHYA_X", "--no-agents", "--no-service")
	if err != nil {
		t.Fatalf("installer with --join: %v; %s", err, joined)
	}
	if got := strings.Count(joined, "--join-code-env\n"); got != 1 {
		t.Fatalf("output = %s; --join-code-env handed to the binary %d times, want 1", joined, got)
	}
	if strings.Contains(joined, "ohya1.invitation.tcfixture") {
		t.Fatalf("output = %s; the code value must not appear", joined)
	}
}

func TestInstallerJoinReadsTerminalWithoutEcho(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the pty harness runs on Linux")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script is not installed")
	}
	root, ca := startFixtureRelease(t)
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	inner := "sh " + script + " --bin-dir " + filepath.Join(dir, "bin") + " --join --no-agents --no-service"
	cmd := exec.CommandContext(ctx, "script", "-qec", inner, "/dev/null")
	cmd.Env = append(os.Environ(), "OHYA_RELEASE_ROOT="+root, "CURL_CA_BUNDLE="+ca, "TMPDIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(4 * time.Second)
		_, _ = stdin.Write([]byte("ohya1.invitation.tcsecret\n"))
		_ = stdin.Close()
	}()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pty installer: %v; output:\n%s", err, output)
	}
	text := strings.ReplaceAll(string(output), "\r", "")
	if !strings.Contains(text, "join code") {
		t.Fatalf("output = %s; want the code prompt", text)
	}
	if !strings.Contains(text, "install\n") || !strings.Contains(text, "--join-code-env\n") || !strings.Contains(text, "OHYA_JOIN_CODE\n") {
		t.Fatalf("output = %s; want the code handed to setup through --join-code-env", text)
	}
	if strings.Contains(text, "ohya1.invitation.tcsecret") {
		t.Fatalf("output = %s; the typed code was echoed", text)
	}
}
