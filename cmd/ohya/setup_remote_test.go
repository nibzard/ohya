package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteSetupPairsAndResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("remote setup transport test")
	}
	dir := transportTestDir(t)
	hubState := filepath.Join(dir, "hub-state")
	ctrl := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	stun := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	var serveLog bytes.Buffer
	server := exec.Command(os.Args[0], "child-serve", "--state", hubState, "--db", filepath.Join(dir, "hub.db"), "--control-listen", ctrl, "--stun-listen", stun)
	server.Stderr = &serveLog
	pipe, err := server.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if server.ProcessState == nil {
			_ = server.Process.Kill()
			_ = server.Wait()
		}
	}()
	lines := make(chan string, 16)
	go func() {
		s := bufio.NewScanner(pipe)
		for s.Scan() {
			select {
			case lines <- s.Text():
			default:
			}
		}
	}()
	waitServeReady(t, lines, &serveLog)
	var invitation bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState}, &invitation, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invitation.String()), "\n", 2)[0]
	t.Setenv("OHYA_SETUP_TEST_CODE", code)
	wrapper := filepath.Join(dir, "ohya-wrapper")
	quoted := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quoted+" child-run \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "setup-state")
	base := []string{"--state", state, "--binary", wrapper, "--no-agents", "--no-service", "--no-modify-path", "--json"}
	first := append(append([]string{}, base...), "--join-code-env", "OHYA_SETUP_TEST_CODE", "--control", "http://"+ctrl)
	var stdout, stderr bytes.Buffer
	if err := runSetupCommand(first, &stdout, &stderr); err != nil {
		t.Fatalf("remote setup: %v; %s %s", err, stdout.String(), stderr.String())
	}
	if os.Getenv("OHYA_SETUP_TEST_CODE") != "" {
		t.Fatal("code remains in process environment")
	}
	if bytes.Contains(stdout.Bytes(), []byte(code)) || bytes.Contains(stderr.Bytes(), []byte(code)) {
		t.Fatal("join code leaked to setup output")
	}
	settings, err := os.ReadFile(filepath.Join(state, "setup", "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(settings, []byte(code)) {
		t.Fatal("join code persisted in setup settings")
	}
	if _, err := os.Stat(filepath.Join(state, "hub.db")); !os.IsNotExist(err) {
		t.Fatal("remote setup created local memory")
	}
	stdout.Reset()
	stderr.Reset()
	if err := runSetupCommand(base, &stdout, &stderr); err != nil {
		t.Fatalf("resume setup: %v; %s %s", err, stdout.String(), stderr.String())
	}
}
