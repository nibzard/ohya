package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func CallMemoryTool(ctx context.Context, command Command, tool string, arguments json.RawMessage) (*mcp.CallToolResult, error) {
	if tool != "sql" && tool != "annotate" {
		return nil, errors.New("only sql and annotate are available")
	}
	if !json.Valid(arguments) {
		return nil, errors.New("tool arguments are not valid JSON")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command.Command, command.Args...)
	cmd.Dir = os.TempDir()
	client := mcp.NewClient(&mcp.Implementation{Name: "ohya-pi-bridge", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	return session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: arguments})
}
