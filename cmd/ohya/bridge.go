package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nibzard/ohya/internal/setup"
)

var bridgeSpec = commandSpec{name: "ohya bridge", synopsis: "call memory tools for a Pi compatibility extension", usageLine: "ohya bridge --connection FILE", description: "Reads one bounded tool request from stdin and returns one MCP tool result. The connection names this executable's MCP command. Used by the owned Pi extension.", examples: []string{"ohya bridge --connection ~/.ohya/setup/pi-bridge.json"}}

func runBridgeCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	c := newCommandFlags(bridgeSpec)
	connection := c.fs.String("connection", "", "owned bridge configuration `file`")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 0 || *connection == "" {
		return c.usageErrorf("--connection is required without positional arguments")
	}
	file, err := os.Open(*connection)
	if err != nil {
		return err
	}
	defer file.Close()
	var command setup.Command
	if err := json.NewDecoder(io.LimitReader(file, 16384)).Decode(&command); err != nil {
		return err
	}
	own, err := os.Executable()
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(command.Command)
	if err != nil {
		return err
	}
	expected, err := filepath.EvalSymlinks(own)
	if err != nil {
		return err
	}
	if actual != expected || len(command.Args) == 0 || command.Args[0] != "mcp" {
		return errors.New("bridge must invoke this executable's MCP command")
	}
	data, err := io.ReadAll(io.LimitReader(stdin, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("bridge request exceeds 1 MiB")
	}
	var request struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return fmt.Errorf("invalid bridge request: %w", err)
	}
	result, err := setup.CallMemoryTool(context.Background(), command, request.Tool, request.Arguments)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
