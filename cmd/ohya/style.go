package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	ansiReset      = "\x1b[0m"
	ansiBold       = "\x1b[1m"
	ansiDim        = "\x1b[2m"
	ansiRed        = "\x1b[31m"
	ansiGreen      = "\x1b[32m"
	ansiYellow     = "\x1b[33m"
	ansiCyan       = "\x1b[36m"
	ansiBoldRed    = "\x1b[1;31m"
	ansiBoldGreen  = "\x1b[1;32m"
	ansiBoldYellow = "\x1b[1;33m"
)

const (
	minFitWidth  = 40
	minFitColumn = 12
)

type cliStyle struct {
	color bool
	width int
}

func styleFor(w io.Writer) cliStyle {
	return cliStyle{color: colorEnabled(w), width: terminalWidth(w)}
}

func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	if force := os.Getenv("FORCE_COLOR"); force != "" && force != "0" {
		return true
	}
	if force := os.Getenv("CLICOLOR_FORCE"); force != "" && force != "0" {
		return true
	}
	return isTerminalWriter(w)
}

func terminalWidth(w io.Writer) int {
	if !isTerminalWriter(w) {
		return 0
	}
	if columns, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && columns > 0 {
		return max(columns, minFitWidth)
	}
	f := w.(*os.File)
	columns, _, err := term.GetSize(int(f.Fd()))
	if err != nil || columns <= 0 {
		return 0
	}
	return max(columns, minFitWidth)
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (s cliStyle) wrap(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return code + strings.ReplaceAll(text, ansiReset, ansiReset+code) + ansiReset
}

func (s cliStyle) wrapHanging(text string, indent int) string {
	if s.width == 0 || s.width-indent < minFitColumn {
		return text
	}
	return strings.ReplaceAll(wrap(text, s.width-indent), "\n", "\n"+strings.Repeat(" ", indent))
}

func (s cliStyle) fit(text string, indent int) string {
	if s.width == 0 {
		return text
	}
	return truncateVisible(text, s.width-indent)
}

func (s cliStyle) bold(text string) string       { return s.wrap(ansiBold, text) }
func (s cliStyle) dim(text string) string        { return s.wrap(ansiDim, text) }
func (s cliStyle) green(text string) string      { return s.wrap(ansiGreen, text) }
func (s cliStyle) yellow(text string) string     { return s.wrap(ansiYellow, text) }
func (s cliStyle) red(text string) string        { return s.wrap(ansiRed, text) }
func (s cliStyle) cyan(text string) string       { return s.wrap(ansiCyan, text) }
func (s cliStyle) boldGreen(text string) string  { return s.wrap(ansiBoldGreen, text) }
func (s cliStyle) boldYellow(text string) string { return s.wrap(ansiBoldYellow, text) }
func (s cliStyle) boldRed(text string) string    { return s.wrap(ansiBoldRed, text) }

func (s cliStyle) section(title string) string {
	return s.bold(title)
}

func (s cliStyle) severityTag(severity string) string {
	if s.color {
		switch severity {
		case sevOK:
			return s.boldGreen("✓")
		case sevWarn:
			return s.boldYellow("▲")
		case sevError:
			return s.boldRed("✗")
		default:
			return s.dim("·")
		}
	}
	return fmt.Sprintf("%-5s", severity)
}

func (s cliStyle) processState(state string, fresh bool) string {
	switch state {
	case "running":
		if s.color {
			return s.green("● running")
		}
		return "running"
	case "stopped":
		if s.color {
			return s.dim("○ stopped")
		}
		return "stopped"
	case "dead":
		if s.color {
			return s.red("✗ dead")
		}
		return "dead"
	case "unknown":
		if s.color {
			if !fresh {
				return s.yellow("▲ stale")
			}
			return s.yellow("? unknown")
		}
		if !fresh {
			return "stale"
		}
		return "unknown"
	default:
		return state
	}
}

func (s cliStyle) serviceState(loaded, running *bool) string {
	switch {
	case running != nil && *running:
		if s.color {
			return s.green("running")
		}
		return "running"
	case loaded != nil && !*loaded:
		if s.color {
			return s.dim("stopped")
		}
		return "stopped"
	case loaded != nil && *loaded:
		if s.color {
			return s.yellow("loaded (not running)")
		}
		return "loaded (not running)"
	default:
		if s.color {
			return s.dim("unknown")
		}
		return "unknown"
	}
}

func (s cliStyle) logLevel(level string) string {
	upper := strings.ToUpper(level)
	padded := fmt.Sprintf("%-5s", upper)
	switch level {
	case "error":
		return s.boldRed(padded)
	case "warn":
		return s.boldYellow(padded)
	case "info":
		return s.cyan(padded)
	default:
		return s.dim(padded)
	}
}

func (s cliStyle) bullet() string {
	if s.color {
		return s.dim("·")
	}
	return "·"
}

func visibleWidth(text string) int {
	width := 0
	for i := 0; i < len(text); {
		if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '[' {
			i += 2
			for i < len(text) && (text[i] < 0x40 || text[i] > 0x7e) {
				i++
			}
			if i < len(text) {
				i++
			}
			continue
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		width++
		i += size
	}
	return width
}

func truncateVisible(text string, width int) string {
	if width <= 0 || visibleWidth(text) <= width {
		return text
	}
	var b strings.Builder
	visible := 0
	styled := false
	for i := 0; i < len(text); {
		if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '[' {
			j := i + 2
			for j < len(text) && (text[j] < 0x40 || text[j] > 0x7e) {
				j++
			}
			if j < len(text) {
				j++
			}
			b.WriteString(text[i:j])
			styled = true
			i = j
			continue
		}
		if visible == width-1 {
			break
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		b.WriteString(text[i : i+size])
		visible++
		i += size
	}
	b.WriteString("…")
	if styled {
		b.WriteString(ansiReset)
	}
	return b.String()
}

func padRight(text string, width int) string {
	pad := width - visibleWidth(text)
	if pad <= 0 {
		return text
	}
	return text + strings.Repeat(" ", pad)
}

func padLeft(text string, width int) string {
	pad := width - visibleWidth(text)
	if pad <= 0 {
		return text
	}
	return strings.Repeat(" ", pad) + text
}

type cliTable struct {
	indent     string
	headers    []string
	rightAlign []bool
	rows       [][]string
}

func (t *cliTable) addRow(cols ...string) {
	t.rows = append(t.rows, cols)
}

func (t *cliTable) render(w io.Writer, s cliStyle) {
	if len(t.rows) == 0 && len(t.headers) == 0 {
		return
	}
	indent := t.indent
	if indent == "" {
		indent = "  "
	}
	numCols := len(t.headers)
	for _, row := range t.rows {
		if len(row) > numCols {
			numCols = len(row)
		}
	}
	widths := make([]int, numCols)
	for i, h := range t.headers {
		widths[i] = visibleWidth(h)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if vw := visibleWidth(cell); vw > widths[i] {
				widths[i] = vw
			}
		}
	}
	if s.width > 0 {
		fitColumns(widths, s.width-visibleWidth(indent))
	}
	writeRow := func(cells []string, isHeader bool) {
		var b strings.Builder
		b.WriteString(indent)
		lastNonEmpty := len(cells) - 1
		for lastNonEmpty >= 0 && cells[lastNonEmpty] == "" {
			lastNonEmpty--
		}
		for i := 0; i <= lastNonEmpty; i++ {
			if i > 0 {
				b.WriteString("  ")
			}
			cell := truncateVisible(cells[i], widths[i])
			rendered := cell
			if isHeader {
				rendered = s.dim(cell)
			}
			right := i < len(t.rightAlign) && t.rightAlign[i]
			if i < lastNonEmpty {
				if right {
					b.WriteString(padLeft(rendered, widths[i]))
				} else {
					b.WriteString(padRight(rendered, widths[i]))
				}
			} else if right {
				b.WriteString(padLeft(rendered, widths[i]))
			} else {
				b.WriteString(rendered)
			}
		}
		fmt.Fprintln(w, b.String())
	}
	if len(t.headers) > 0 {
		writeRow(t.headers, true)
	}
	for _, row := range t.rows {
		writeRow(row, false)
	}
}

func fitColumns(widths []int, available int) {
	if len(widths) == 0 {
		return
	}
	total := 2 * (len(widths) - 1)
	for _, width := range widths {
		total += width
	}
	last := len(widths) - 1
	if excess := total - available; excess > 0 {
		widths[last] = max(widths[last]-excess, min(widths[last], minFitColumn))
	}
}

func writeKeyValues(w io.Writer, s cliStyle, indent string, pairs [][2]string) {
	maxKey := 0
	for _, p := range pairs {
		if vw := visibleWidth(p[0]); vw > maxKey {
			maxKey = vw
		}
	}
	for _, p := range pairs {
		label := padRight(s.dim(p[0]), maxKey)
		fmt.Fprintf(w, "%s%s  %s\n", indent, label, p[1])
	}
}

func writeStatusFindingsText(w io.Writer, s cliStyle, findings []statusFinding) {
	if len(findings) == 0 {
		return
	}
	if len(findings) == 1 && findings[0].Severity == sevOK {
		f := findings[0]
		fmt.Fprintf(w, "\n%s %s %s\n", s.severityTag(f.Severity), s.bold(f.Code+":"), f.Evidence)
		return
	}
	fmt.Fprintf(w, "\n%s\n", s.section("Findings"))
	hintIndent := "        "
	if s.color {
		hintIndent = "    "
	}
	seenHints := map[string]bool{}
	for _, f := range findings {
		fmt.Fprintf(w, "  %s %s %s\n", s.severityTag(f.Severity), s.bold(f.Code+":"), f.Evidence)
		if f.Hint != "" && !seenHints[f.Hint] {
			seenHints[f.Hint] = true
			fmt.Fprintf(w, "%s%s\n", hintIndent, s.dim("→ "+f.Hint))
		}
	}
}

func serviceDisplayName(name string) string {
	switch name {
	case "service":
		return "memory"
	case "hub-service":
		return "hub"
	default:
		return name
	}
}
