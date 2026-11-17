package main

import (
	"bytes"
	"os"
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
	hubDB := filepath.Join(dir, "hub.db")
	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, _, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	defer func() {
		if serveCmd.ProcessState == nil {
			_ = serveCmd.Process.Kill()
			serveCmd.Wait()
		}
	}()
	code := inviteCode(t, hubState)
	t.Setenv("OHYA_SETUP_TEST_CODE", code)
	wrapper := filepath.Join(dir, "ohya-wrapper")
	quoted := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quoted+" child-run \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "setup-state")
	base := []string{"--state", state, "--binary", wrapper, "--no-agents", "--no-service", "--no-modify-path", "--json"}
	var stdout, stderr bytes.Buffer
	first := append(append([]string{}, base...), "--join-code-env", "OHYA_SETUP_TEST_CODE")
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
	if _, err := os.Stat(filepath.Join(state, "join", "credential.json")); err != nil {
		t.Fatal("remote setup paired no device")
	}
	if _, err := os.Stat(filepath.Join(state, "join", "hub-address.json")); err != nil {
		t.Fatal("remote setup saved no hub address")
	}
	stdout.Reset()
	stderr.Reset()
	if err := runSetupCommand(base, &stdout, &stderr); err != nil {
		t.Fatalf("resume setup: %v; %s %s", err, stdout.String(), stderr.String())
	}
}
