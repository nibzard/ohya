package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Client struct {
	Name       string `json:"name"`
	Executable string `json:"executable,omitempty"`
	Config     string `json:"config,omitempty"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
}

type ClientOptions struct {
	Home     string
	State    string
	Binary   string
	Args     []string
	Agents   string
	Getenv   func(string) string
	LookPath func(string) (string, error)
}

func DiscoverClients(o ClientOptions) ([]Client, error) {
	if !filepath.IsAbs(o.Home) || !filepath.IsAbs(o.State) || !filepath.IsAbs(o.Binary) {
		return nil, errors.New("setup needs absolute home, state, and executable paths")
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	names := []string{"claude", "codex", "pi"}
	explicit := o.Agents != "" && o.Agents != "auto"
	if explicit {
		names = strings.Split(o.Agents, ",")
	}
	seen := map[string]bool{}
	var clients []Client
	for _, name := range names {
		if name != "claude" && name != "codex" && name != "pi" {
			return nil, fmt.Errorf("unsupported agent %q; choose claude,codex,pi", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("agent %s is selected twice", name)
		}
		seen[name] = true
		c := Client{Name: name, Status: "planned"}
		path, err := o.LookPath(name)
		if err != nil {
			c.Status = "absent"
			if explicit {
				c.Status = "failed"
				c.Detail = "requested agent is not installed"
			}
			clients = append(clients, c)
			continue
		}
		c.Executable, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		switch name {
		case "claude":
			dir := o.Getenv("CLAUDE_CONFIG_DIR")
			if dir == "" {
				dir = o.Home
			}
			c.Config = filepath.Join(dir, ".claude.json")
		case "codex":
			dir := o.Getenv("CODEX_HOME")
			if dir == "" {
				dir = filepath.Join(o.Home, ".codex")
			}
			c.Config = filepath.Join(dir, "config.toml")
		case "pi":
			dir := o.Getenv("PI_CODING_AGENT_DIR")
			if dir == "" {
				dir = filepath.Join(o.Home, ".pi", "agent")
			}
			c.Config = filepath.Join(dir, "mcp.json")
		}
		if !filepath.IsAbs(c.Config) {
			return nil, fmt.Errorf("%s configuration override must be absolute", name)
		}
		clients = append(clients, c)
	}
	return clients, nil
}

func ApplyClient(o ClientOptions, c Client) (bool, error) {
	receipt := filepath.Join(o.State, "setup", "clients", c.Name+".json")
	command := Command{Command: o.Binary, Args: o.Args}
	if c.Name == "codex" {
		return ApplyTOMLEntry(c.Config, receipt, command)
	}
	if c.Name != "claude" && c.Name != "pi" {
		return false, fmt.Errorf("unsupported client %s", c.Name)
	}
	value, err := json.Marshal(struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}{Type: "stdio", Command: o.Binary, Args: o.Args})
	if err != nil {
		return false, err
	}
	return ApplyJSONEntry(JSONEntry{Path: c.Config, Parent: "mcpServers", Name: "ohya", Value: value}, receipt)
}

func RemoveClient(state, name string) (bool, error) {
	if name != "claude" && name != "codex" && name != "pi" {
		return false, fmt.Errorf("unsupported client %s", name)
	}
	receipt := filepath.Join(state, "setup", "clients", name+".json")
	if name == "codex" {
		return RemoveTOMLEntry(receipt)
	}
	return RemoveJSONEntry(receipt)
}
