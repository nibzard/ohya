package main

import (
	"io"
	"slices"
	"testing"
)

func TestParseInstallArgsAgreesWithSetup(t *testing.T) {
	for _, test := range []struct {
		args    []string
		wantDry bool
	}{
		{args: []string{"-dry-run"}, wantDry: true},
		{args: []string{"--dry-run"}, wantDry: true},
		{args: []string{"-dry-run=TRUE", "--"}, wantDry: true},
		{args: []string{"--dry-run=1", "--no-agents"}, wantDry: true},
		{args: []string{"--dry-run", "-dry-run=false"}},
		{args: []string{"--dry-run", "--dry-run=0"}},
		{args: []string{"--state", "-dry-run"}},
	} {
		_, _, dry, setupArgs, err := parseInstallArgs(test.args, "/default/bin")
		if err != nil {
			t.Fatalf("parseInstallArgs(%q): %v", test.args, err)
		}
		c, flags := newSetupFlags("", "")
		if err := c.parse(setupArgs, io.Discard); err != nil {
			t.Fatalf("setup parse(%q): %v", setupArgs, err)
		}
		if dry != test.wantDry || *flags.dry != test.wantDry {
			t.Fatalf("args %q: install dry run = %v, setup dry run = %v, want %v", test.args, dry, *flags.dry, test.wantDry)
		}
	}
}

func TestParseInstallArgsForwardsSetupFlags(t *testing.T) {
	args := []string{"-bin-dir", "/opt/bin", "--exclude", "old", "-version=v1.2.3",
		"--exclude=", "--exclude", "first", "-dry-run", "--exclude", "second"}
	dir, version, dry, setupArgs, err := parseInstallArgs(args, "/default/bin")
	if err != nil || dir != "/opt/bin" || version != "v1.2.3" || !dry {
		t.Fatalf("parseInstallArgs = %q, %q, %v, %v", dir, version, dry, err)
	}
	c, flags := newSetupFlags("", "")
	if err := c.parse(setupArgs, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !*flags.dry || !slices.Equal(*flags.excludes, []string{"first", "second"}) {
		t.Fatalf("setup dry run = %v, excludes = %q", *flags.dry, *flags.excludes)
	}
}

func TestParseInstallArgsRejectsInvalidFlagsBeforeInstall(t *testing.T) {
	for _, args := range [][]string{
		{"-bin-dir"}, {"--version"}, {"-dry-run=invalid"},
		{"---dry-run"}, {"--", "-dry-run"}, {"--no-such-flag", "-dry-run"},
	} {
		if _, _, _, _, err := parseInstallArgs(args, "/default/bin"); err == nil {
			t.Fatalf("parseInstallArgs(%q) succeeded", args)
		}
	}
}
