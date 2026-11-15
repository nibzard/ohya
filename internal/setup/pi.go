package setup

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"text/template"

	"github.com/nibzard/ohya/internal/mcpserver"
)

//go:embed pi-extension.js.tmpl
var piExtension string

func ApplyPiExtension(o ClientOptions, c Client) (bool, error) {
	config := filepath.Join(o.State, "setup", "pi-bridge.json")
	raw, err := json.Marshal(Command{Command: o.Binary, Args: o.Args})
	if err != nil {
		return false, err
	}
	configChanged, err := ApplyFile(config, filepath.Join(o.State, "setup", "pi-bridge-receipt.json"), raw)
	if err != nil {
		return false, err
	}
	encode := func(s string) string { raw, _ := json.Marshal(s); return string(raw) }
	values := struct{ Executable, Connection, Guidance string }{encode(o.Binary), encode(config), encode(mcpserver.MemoryInstructions)}
	parsed, err := template.New("pi").Parse(piExtension)
	if err != nil {
		return false, err
	}
	var content bytes.Buffer
	if err := parsed.Execute(&content, values); err != nil {
		return false, err
	}
	path := filepath.Join(filepath.Dir(c.Config), "extensions", "ohya-memory.js")
	changed, err := ApplyFile(path, filepath.Join(o.State, "setup", "pi-extension.json"), content.Bytes())
	return changed || configChanged, err
}

func RemovePiExtension(state string) (bool, error) {
	changed, err := RemoveFile(filepath.Join(state, "setup", "pi-extension.json"))
	if err != nil {
		return changed, err
	}
	configChanged, err := RemoveFile(filepath.Join(state, "setup", "pi-bridge-receipt.json"))
	return changed || configChanged, err
}
