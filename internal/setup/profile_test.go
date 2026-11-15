package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfilesPreserveUserContentAndShellLiterals(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	original := "export OHYA_TEST_VALUE=kept\n"
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "quote' and $dollar [glob] space")
	plans, err := ProfilePlan(home, "/bin/bash", bin)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[1].Path != profile {
		t.Fatal(plans)
	}
	for _, p := range plans {
		if changed, err := ApplyProfile(state, p); err != nil || !changed {
			t.Fatalf("apply %v %v", changed, err)
		}
		if changed, err := ApplyProfile(state, p); err != nil || changed {
			t.Fatalf("repeat %v %v", changed, err)
		}
	}
	cmd := exec.Command("sh", "-c", `. "$1"; . "$1"; printf '%s' "$PATH"`, "sh", profile)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	result, err := cmd.Output()
	if err != nil || string(result) != bin+":/usr/bin:/bin" {
		t.Fatalf("shell PATH = %s %v", result, err)
	}
	f, err := os.OpenFile(profile, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("export AFTER_SETUP=yes\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	results, err := RemoveProfiles(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Status != "removed" {
			t.Fatal(r)
		}
	}
	restored, err := os.ReadFile(profile)
	if err != nil || string(restored) != original+"export AFTER_SETUP=yes\n" {
		t.Fatalf("restored %q %v", restored, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".bashrc")); !os.IsNotExist(err) {
		t.Fatal("created rc remains")
	}
}

func TestProfilesProtectEditedBlocksAndExistingFishFile(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	plans, err := ProfilePlan(home, "zsh", "/bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProfile(state, plans[0]); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(plans[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "export PATH=", "export MY_PATH=", 1)
	if err := os.WriteFile(plans[0].Path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProfile(state, plans[0]); err == nil {
		t.Fatal("overwrote edited block")
	}
	results, err := RemoveProfiles(state)
	if err != nil || len(results) != 1 || results[0].Status != "failed" {
		t.Fatalf("remove %v %v", results, err)
	}
	fish, err := ProfilePlan(home, "fish", "/bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProfile(state, fish[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveFile(profileReceiptPath(state, fish[0].Path)); err != nil {
		t.Fatal(err)
	}
	if _, err := ProfilePlan(home, "unknown", "/bin"); err == nil {
		t.Fatal("accepted unknown shell")
	}
}
