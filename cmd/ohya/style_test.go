package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestStyleRespectsNoColorAndDumbTerm(t *testing.T) {
	var buf bytes.Buffer
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	t.Setenv("TERM", "xterm-256color")
	if styleFor(&buf).color {
		t.Fatal("non-TTY buffer must not emit ANSI color")
	}

	t.Setenv("FORCE_COLOR", "1")
	if !styleFor(&buf).color {
		t.Fatal("FORCE_COLOR=1 must enable color")
	}

	t.Setenv("NO_COLOR", "1")
	if styleFor(&buf).color {
		t.Fatal("NO_COLOR must override FORCE_COLOR")
	}

	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if styleFor(&buf).color {
		t.Fatal("TERM=dumb must disable color")
	}
}

func TestVisibleWidthIgnoresANSI(t *testing.T) {
	plain := cliStyle{color: false}
	colored := cliStyle{color: true}
	if got := visibleWidth(colored.boldGreen("● running")); got != visibleWidth(plain.boldGreen("● running")) {
		t.Fatalf("visibleWidth = %d, want %d", got, visibleWidth("● running"))
	}
}

func TestCLITableAlignsPlainAndColoredEqually(t *testing.T) {
	render := func(s cliStyle) string {
		var tbl cliTable
		tbl.headers = []string{"ROLE", "PID", "STATE"}
		tbl.rightAlign = []bool{false, true, false}
		tbl.addRow(s.cyan("ingest"), "42", s.processState("running", true))
		tbl.addRow(s.cyan("serve"), "10000", s.processState("dead", false))
		var out bytes.Buffer
		tbl.render(&out, s)
		return out.String()
	}

	plain := render(cliStyle{color: false})
	if strings.ContainsRune(plain, 0x1b) {
		t.Fatalf("plain table leaked ANSI escape: %q", plain)
	}
	lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), plain)
	}
	if strings.Index(lines[1], "running") != strings.Index(lines[2], "dead") {
		t.Fatalf("columns misaligned:\n%s", plain)
	}

	colored := render(cliStyle{color: true})
	if !strings.ContainsRune(colored, 0x1b) {
		t.Fatalf("colored table must include ANSI styling: %q", colored)
	}
	clines := strings.Split(strings.TrimSuffix(colored, "\n"), "\n")
	if visibleWidth(clines[0]) != visibleWidth("  ROLE      PID  STATE") {
		t.Fatalf("header width mismatch: %q", clines[0])
	}
}
