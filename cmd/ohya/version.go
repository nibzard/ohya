// The version command: one line naming the build and the data it writes.
// Operators and agents compare a hub's parser output versions and schema
// version before acting on its rows — a build refuses rows derived by a
// newer parser (internal/store/rederive.go) — so the line must be readable
// without opening any database.

package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

// versionSpec is the version usage template.
var versionSpec = commandSpec{
	name:      "ohya version",
	synopsis:  "print the build, parser, and schema versions",
	usageLine: "ohya version [flags]",
	description: "Prints one line of space-separated key=value pairs: the build identity (version, revision, dirty, go), the schema version this build migrates a database to, and every parser's output version. " +
		"The command opens no database and needs no state.\n" +
		"Compare a hub's line with the local one before rederiving: a build refuses rows derived by a newer parser, and the parser keys tell the two binaries apart.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya version", "ohya --version"},
}

// isVersionArg matches the --version spellings the root command accepts, the
// same one-or-two-dash rule as help.
func isVersionArg(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	return name != arg && name == "version"
}

// runVersionCommand prints the version line; `ohya --version` is an alias.
func runVersionCommand(args []string, stdout, stderr io.Writer) error {
	c := newCommandFlags(versionSpec)
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	line, err := versionLine()
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, line)
	return nil
}

// versionLine builds the one-line version report: the program name, then
// space-separated key=value pairs in a fixed order. Values come from the
// build metadata go build embeds, the newest embedded migration, and each
// parser's OutputVersion constant.
func versionLine() (string, error) {
	version, revision, dirty, goVer := buildIdentity()
	schema, err := store.SchemaVersion()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ohya version=%s revision=%s dirty=%s go=%s schema=%d parser.claude=%d parser.codex=%d parser.pi=%d\n",
		version, revision, dirty, goVer, schema,
		claudecode.OutputVersion, codex.OutputVersion, pi.OutputVersion), nil
}

// buildIdentity extracts the build identity from runtime/debug.ReadBuildInfo,
// which go build embeds without ldflags. A binary with no module version
// reports version=devel; one built outside a version-controlled checkout
// reports unknown revision and dirty state.
func buildIdentity() (version, revision, dirty, goVer string) {
	version, revision, dirty, goVer = "devel", "unknown", "unknown", runtime.Version()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	goVer = info.GoVersion
	if v := info.Main.Version; v != "" && v != "(devel)" {
		version = v
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				revision = shortRevision(s.Value)
			}
		case "vcs.modified":
			dirty = s.Value
		}
	}
	return
}

// shortRevision truncates a revision to the short-SHA length; a shorter value
// stays whole.
func shortRevision(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
