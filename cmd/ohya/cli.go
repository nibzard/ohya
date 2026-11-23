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

var errHelp = errors.New("help shown")

type usageError struct {
	cmd string
	msg string
}

func (e usageError) Error() string { return e.msg }

func cliMain(args []string, stdout, stderr io.Writer) int {
	err := run(args, stdout, stderr)
	if err == nil {
		return 0
	}
	s := styleFor(stderr)
	var ue usageError
	switch {
	case errors.As(err, &ue):
		if s.color {
			fmt.Fprintf(stderr, "%s %s\n  %s\n", s.boldRed("error:"), ue.msg, s.dim(fmt.Sprintf("run '%s --help'", ue.cmd)))
		} else {
			fmt.Fprintf(stderr, "ohya: %s; run '%s --help'\n", ue.msg, ue.cmd)
		}
		return 2
	case errors.Is(err, errHelp):
		return 0
	case errors.Is(err, errDegraded):
		return 3
	default:
		if s.color {
			fmt.Fprintf(stderr, "%s %v\n", s.boldRed("error:"), err)
		} else {
			fmt.Fprintln(stderr, "ohya:", err)
		}
		return 1
	}
}

func isHelpArg(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	return name != arg && (name == "help" || name == "h")
}

type commandSpec struct {
	name        string
	synopsis    string
	usageLine   string
	description string
	argsTitle   string
	args        string
	examples    []string
}

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

func (c *commandFlagSet) parse(args []string, stdout io.Writer) error {
	permuted, valueless := permuteFlags(c.fs, args)
	if valueless != "" {
		if err := c.fs.Parse([]string{valueless}); err != nil {
			return usageError{cmd: c.spec.name, msg: err.Error()}
		}
	}
	if err := c.fs.Parse(permuted); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			c.buf.WriteTo(stdout)
			return errHelp
		}
		return usageError{cmd: c.spec.name, msg: err.Error()}
	}
	return nil
}

func permuteFlags(fs *flag.FlagSet, args []string) (permuted []string, valuelessFlag string) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		if arg == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		flags = append(flags, arg)
		if strings.Contains(arg, "=") {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 >= len(args) {
			return nil, arg
		}
		i++
		flags = append(flags, args[i])
	}
	return append(flags, positional...), ""
}

func (c *commandFlagSet) usageErrorf(format string, args ...any) error {
	return usageError{cmd: c.spec.name, msg: fmt.Sprintf(format, args...)}
}

func writeUsage(w io.Writer, spec commandSpec, fs *flag.FlagSet) {
	s := styleFor(w)
	fmt.Fprintf(w, "%s %s %s\n\n", s.bold(spec.name), s.dim("—"), spec.synopsis)
	fmt.Fprintf(w, "%s\n\n  %s\n\n", s.bold("Usage:"), s.cyan(spec.usageLine))
	fmt.Fprintf(w, "%s\n\n", wrap(spec.description, 80))
	if spec.args != "" {
		title := spec.argsTitle
		if title == "" {
			title = "Arguments"
		}
		fmt.Fprintf(w, "%s\n\n", s.bold(title+":"))
		for _, line := range strings.Split(spec.args, "\n") {
			if line != "" {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%s\n\n", s.bold("Flags:"))
	renderFlags(w, s, fs)
	fmt.Fprintf(w, "\n%s\n\n", s.bold("Examples:"))
	for _, ex := range spec.examples {
		fmt.Fprintf(w, "  %s\n", s.dim(ex))
	}
}

func renderFlags(w io.Writer, s cliStyle, fs *flag.FlagSet) {
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
		styledName := padRight(s.cyan(r.name), col)
		prefix := "  " + styledName + "  "
		indent := 2 + col + 2
		lines := strings.Split(wrap(r.doc, 80-indent), "\n")
		if r.suffix != "" {
			last := len(lines) - 1
			if utf8.RuneCountInString(lines[last])+1+utf8.RuneCountInString(r.suffix) <= 80-indent {
				lines[last] += " " + s.dim(r.suffix)
			} else {
				lines = append(lines, s.dim(r.suffix))
			}
		}
		for _, line := range lines {
			fmt.Fprintf(w, "%s%s\n", prefix, line)
			prefix = strings.Repeat(" ", indent)
		}
	}
}

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

func wrap(text string, width int) string {
	var paragraphs []string
	for _, para := range strings.Split(text, "\n") {
		if strings.TrimSpace(para) == "" {
			continue
		}
		var out []string
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
		paragraphs = append(paragraphs, strings.Join(out, "\n"))
	}
	return strings.Join(paragraphs, "\n\n")
}
