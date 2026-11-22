package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/nibzard/ohya/internal/antigravity"
	"github.com/nibzard/ohya/internal/buildinfo"
	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

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

func isVersionArg(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	return name != arg && name == "version"
}

func runVersionCommand(args []string, stdout, stderr io.Writer) error {
	c := newCommandFlags(versionSpec)
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of the text line (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if *asJSON {
		return versionJSON(stdout)
	}
	line, err := versionLine()
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, line)
	return nil
}

type versionJSONData struct {
	Version           string `json:"version"`
	Revision          string `json:"revision"`
	Dirty             string `json:"dirty"`
	Go                string `json:"go"`
	Schema            int    `json:"schema"`
	ParserClaude      int    `json:"parser.claude"`
	ParserCodex       int    `json:"parser.codex"`
	ParserPi          int    `json:"parser.pi"`
	ParserAntigravity int    `json:"parser.antigravity"`
}

func versionJSON(stdout io.Writer) error {
	schema, err := store.SchemaVersion()
	if err != nil {
		return printJSONError(stdout, versionSpec.name, err)
	}
	version, revision, dirty, goVer := buildinfo.Identity()
	return printJSON(stdout, versionSpec.name, versionJSONData{
		Version:           version,
		Revision:          revision,
		Dirty:             dirty,
		Go:                goVer,
		Schema:            schema,
		ParserClaude:      claudecode.OutputVersion,
		ParserCodex:       codex.OutputVersion,
		ParserPi:          pi.OutputVersion,
		ParserAntigravity: antigravity.OutputVersion,
	})
}

func versionLine() (string, error) {
	version, revision, dirty, goVer := buildinfo.Identity()
	schema, err := store.SchemaVersion()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ohya version=%s revision=%s dirty=%s go=%s schema=%d parser.claude=%d parser.codex=%d parser.pi=%d parser.antigravity=%d\n",
		version, revision, dirty, goVer, schema,
		claudecode.OutputVersion, codex.OutputVersion, pi.OutputVersion, antigravity.OutputVersion), nil
}
