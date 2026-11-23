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

func TestStyleLeavesNonTerminalWidthUnbounded(t *testing.T) {
	t.Setenv("COLUMNS", "50")
	if got := styleFor(&bytes.Buffer{}).width; got != 0 {
		t.Fatalf("non-TTY width = %d; want 0 so piped output is never cut", got)
	}
}

func TestTruncateVisibleKeepsANSIBalanced(t *testing.T) {
	s := cliStyle{color: true}
	cases := []struct {
		text  string
		width int
		want  string
	}{
		{"abcdef", 10, "abcdef"},
		{"abcdef", 6, "abcdef"},
		{"abcdef", 4, "abc…"},
		{"héllo wörld", 5, "héll…"},
	}
	for _, tc := range cases {
		if got := truncateVisible(tc.text, tc.width); got != tc.want {
			t.Fatalf("truncateVisible(%q, %d) = %q; want %q", tc.text, tc.width, got, tc.want)
		}
	}
	got := truncateVisible(s.dim("abcdef"), 4)
	if visibleWidth(got) != 4 || !strings.HasSuffix(got, "…"+ansiReset) {
		t.Fatalf("styled truncation = %q; want 4 visible cells ending in a reset", got)
	}
}

func TestNestedStyleRestoresOuterCode(t *testing.T) {
	s := cliStyle{color: true}
	got := s.dim("a " + s.bold("b") + " c")
	want := ansiDim + "a " + ansiBold + "b" + ansiReset + ansiDim + " c" + ansiReset
	if got != want {
		t.Fatalf("nested style = %q; want %q", got, want)
	}
}

func TestCLITableFitsOnlyTheLastColumn(t *testing.T) {
	s := cliStyle{width: 40}
	tbl := cliTable{headers: []string{"ID", "TEXT"}}
	tbl.addRow("claude-code:dev1:session-0001", strings.Repeat("word ", 20))
	var out bytes.Buffer
	tbl.render(&out, s)
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if strings.Contains(line, "claude-code") && !strings.Contains(line, "claude-code:dev1:session-0001") {
			t.Fatalf("identifier column was cut: %q", line)
		}
	}
	if !strings.Contains(out.String(), "…") {
		t.Fatalf("last column was not fitted:\n%s", out.String())
	}
}

func TestWrapHangingIndentsContinuationLines(t *testing.T) {
	s := cliStyle{width: 30}
	got := s.wrapHanging("alpha beta gamma delta epsilon zeta", 10)
	want := "alpha beta gamma\n          delta epsilon zeta"
	if got != want {
		t.Fatalf("wrapHanging = %q; want %q", got, want)
	}
	if got := (cliStyle{}).wrapHanging("alpha beta", 10); got != "alpha beta" {
		t.Fatalf("unbounded width must not wrap: %q", got)
	}
}
