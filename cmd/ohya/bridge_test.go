package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/setup"
)

func TestBridgeRejectsUnsafeCommandsAndRequests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.json")
	for _, test := range []struct {
		args    []string
		request string
	}{{[]string{"version"}, `{"tool":"sql","arguments":{}}`}, {[]string{"mcp"}, strings.Repeat("x", (1<<20)+1)}, {[]string{"mcp"}, `{"tool":"execute","arguments":{}}`}} {
		raw, err := json.Marshal(setup.Command{Command: os.Args[0], Args: test.args})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runBridgeCommand([]string{"--connection", path}, strings.NewReader(test.request), &out); err == nil {
			t.Fatal("accepted unsafe request")
		}
	}
}
