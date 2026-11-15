package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func VerifyCommand(ctx context.Context, command Command) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command.Command, command.Args...)
	cmd.Dir = os.TempDir()
	client := mcp.NewClient(&mcp.Implementation{Name: "ohya-setup", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}, nil)
	if err != nil {
		return fmt.Errorf("initialize MCP: %w", err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("discover MCP tools: %w", err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	if !names["sql"] || !names["annotate"] {
		return errors.New("MCP server does not expose sql and annotate")
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sql", Arguments: map[string]any{"query": "SELECT 1 AS ohya_setup_check"}})
	if err != nil {
		return fmt.Errorf("query MCP: %w", err)
	}
	if result.IsError {
		return errors.New("MCP setup query failed; run ohya doctor")
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	var payload struct {
		Columns []string `json:"columns"`
		Rows    [][]int  `json:"rows"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return errors.New("MCP setup query returned an invalid result")
	}
	if len(payload.Columns) != 1 || payload.Columns[0] != "ohya_setup_check" || len(payload.Rows) != 1 || len(payload.Rows[0]) != 1 || payload.Rows[0][0] != 1 {
		return errors.New("MCP setup query returned an unexpected result")
	}
	return nil
}
