// The CLI contract (T059): help is a request — every level prints the same
// usage template on stdout and exits 0 — while a usage error prints one line
// plus a pointer to --help on stderr and exits 2, the flag package
// convention. Every other failure keeps exit 1, except a degraded
// `ingest --once` pass (a source failed or stopped), which exits 3 with its
// diagnostics already on stderr (T060). Everything runs through the
// standard library flag package; no CLI framework (MANIFESTO §14,
// ADR 0001). No command prompts for input: ohya is non-interactive by
// design.

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// errHelp marks a rendered help request: the usage text is already on
// stdout, and the process exits 0 with no error line.
var errHelp = errors.New("help shown")

// usageError marks a usage failure: unknown subcommand, bad flag, missing
// or unexpected argument. The process prints one line naming the problem
// plus a pointer to cmd's --help on stderr and exits 2.
type usageError struct {
	cmd string // the command whose --help answers this failure
	msg string
}

func (e usageError) Error() string { return e.msg }

// cliMain runs one command and returns the process exit code: 0 for success
// and for help, 2 for usage errors (one stderr line plus a --help pointer),
// 3 for a degraded `ingest --once` pass (a source failed or stopped; the
// diagnostics are already on stderr, so no error line is added), 1 for
// failures.
func cliMain(args []string, stdout, stderr io.Writer) int {
	err := run(args, stdout, stderr)
	if err == nil {
		return 0
	}
	var ue usageError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "ohya: %s; run '%s --help'\n", ue.msg, ue.cmd)
		return 2
	case errors.Is(err, errHelp):
		return 0
	case errors.Is(err, errDegraded):
		return 3
	default:
		fmt.Fprintln(stderr, "ohya:", err)
		return 1
	}
}

// isHelpArg matches the help spellings the flag package accepts: one or two
// dashes before "help" or "h". The parser keeps accepting one dash, but the
// help text and the docs spell flags with two.
func isHelpArg(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	return name != arg && (name == "help" || name == "h")
}

// commandSpec describes one command level for the usage template every
// level shares: usage line, one-paragraph description, arguments or
// commands, flags, and one or two examples.
type commandSpec struct {
	name        string // "ohya ingest"
	synopsis    string // one line after the em dash
	usageLine   string // "ohya ingest [flags]"
	description string // one paragraph; wrapped to 80 columns when rendered
	argsTitle   string // "Commands" at dispatch levels, "Arguments" at leaves
	args        string // argument or command block; rendered verbatim
	examples    []string
}

// commandFlagSet ties one command's FlagSet to its spec. The flag package
// renders the template into buf from inside Parse — a help request and a
// parse failure both call Usage — and the caller decides afterwards which
// stream the text reaches: stdout for help, nowhere for a failure. The
// package's own output is discarded, so this package owns every printed
// byte.
type commandFlagSet struct {
	spec commandSpec
	fs   *flag.FlagSet
	buf  bytes.Buffer
}

func newCommandFlags(spec commandSpec) *commandFlagSet {
	c := &commandFlagSet{spec: spec, fs: flag.NewFlagSet(spec.name, flag.ContinueOnError)}
	c.fs.SetOutput(io.Discard)
	c.fs.Usage = func() { writeUsage(&c.buf, spec, c.fs) }
	return c
}

// parse parses args under the CLI contract: a help request flushes the
// buffered usage text to stdout and reports errHelp (exit 0); every other
// parse failure is a usage error (exit 2).
func (c *commandFlagSet) parse(args []string, stdout io.Writer) error {
	if err := c.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			c.buf.WriteTo(stdout)
			return errHelp
		}
		return usageError{cmd: c.spec.name, msg: err.Error()}
	}
	return nil
}

// usageErrorf formats a post-parse usage failure, for example a missing
// positional argument, with the same contract as a parse failure.
func (c *commandFlagSet) usageErrorf(format string, args ...any) error {
	return usageError{cmd: c.spec.name, msg: fmt.Sprintf(format, args...)}
}

// writeUsage renders the shared template. A nil fs means a dispatch level,
// which defines no flags of its own beyond the universal --help.
func writeUsage(w io.Writer, spec commandSpec, fs *flag.FlagSet) {
	fmt.Fprintf(w, "%s — %s\n\n", spec.name, spec.synopsis)
	fmt.Fprintf(w, "Usage:\n\n  %s\n\n", spec.usageLine)
	fmt.Fprintf(w, "%s\n\n", wrap(spec.description, 80))
	fmt.Fprintf(w, "%s:\n\n", spec.argsTitle)
	for _, line := range strings.Split(spec.args, "\n") {
		if line != "" {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	fmt.Fprintf(w, "\nFlags:\n\n")
	renderFlags(w, fs)
	fmt.Fprintf(w, "\nExamples:\n\n")
	for _, ex := range spec.examples {
		fmt.Fprintf(w, "  %s\n", ex)
	}
}

// renderFlags writes the flag block: two-dash spellings, the value name
// from the usage backticks, the default when it differs from the type's
// zero value, and the --help every command takes. A default suffix joins
// the wrapped usage when it fits and otherwise takes its own line, so a
// quoted default path never breaks in two.
func renderFlags(w io.Writer, fs *flag.FlagSet) {
	type flagRow struct{ name, doc, suffix string }
	var rows []flagRow
	if fs != nil {
		fs.VisitAll(func(f *flag.Flag) {
			valueName, usage := flag.UnquoteUsage(f)
			left := "--" + f.Name
			if valueName != "" {
				left += " " + valueName
			}
			rows = append(rows, flagRow{left, usage, defaultSuffix(f)})
		})
	}
	rows = append(rows, flagRow{"--help", "show this help and exit", ""})
	col := 0
	for _, r := range rows {
		col = max(col, len(r.name))
	}
	for _, r := range rows {
		prefix := fmt.Sprintf("  %-*s  ", col, r.name)
		indent := utf8.RuneCountInString(prefix)
		lines := strings.Split(wrap(r.doc, 80-indent), "\n")
		if r.suffix != "" {
			last := len(lines) - 1
			if utf8.RuneCountInString(lines[last])+1+utf8.RuneCountInString(r.suffix) <= 80-indent {
				lines[last] += " " + r.suffix
			} else {
				lines = append(lines, r.suffix)
			}
		}
		for _, line := range lines {
			fmt.Fprintf(w, "%s%s\n", prefix, line)
			prefix = strings.Repeat(" ", indent)
		}
	}
}

// defaultSuffix names a flag's default when it differs from the zero value
// of its type, mirroring the flag package's own rule; string defaults are
// quoted.
func defaultSuffix(f *flag.Flag) string {
	typ := reflect.TypeOf(f.Value)
	if typ == nil || typ.Kind() != reflect.Ptr {
		return ""
	}
	zero, ok := reflect.New(typ.Elem()).Interface().(flag.Value)
	if ok && zero.String() == f.DefValue {
		return ""
	}
	if typ.Elem().Kind() == reflect.String {
		return fmt.Sprintf("(default %q)", f.DefValue)
	}
	return fmt.Sprintf("(default %s)", f.DefValue)
}

// wrap breaks text into lines of at most width columns of content on
// spaces. A word longer than width stays whole; callers indent the lines
// they print.
func wrap(text string, width int) string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		var line string
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
