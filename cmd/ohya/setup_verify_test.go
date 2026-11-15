package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nibzard/ohya/internal/setup"
)

func TestSetupVerifiesRealMemoryTools(t *testing.T) {
	dir := t.TempDir()
	command := setup.Command{Command: os.Args[0], Args: []string{"child-run", "mcp", "--db", filepath.Join(dir, "hub.db"), "--state", dir}}
	if err := setup.VerifyCommand(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	command.Args = []string{"child-run", "version"}
	if err := setup.VerifyCommand(context.Background(), command); err == nil {
		t.Fatal("accepted a command that does not serve MCP")
	}
}
