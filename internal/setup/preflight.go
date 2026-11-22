package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
)

type mcpRule struct {
	Name    string   `json:"serverName"`
	Command []string `json:"serverCommand"`
}

type claudePolicy struct {
	Allowed     []mcpRule `json:"allowedMcpServers"`
	Denied      []mcpRule `json:"deniedMcpServers"`
	ManagedOnly bool      `json:"allowManagedMcpServersOnly"`
}

func ruleMatches(r mcpRule, command Command) bool {
	return r.Name == "ohya" || (len(r.Command) > 0 && reflect.DeepEqual(r.Command, append([]string{command.Command}, command.Args...)))
}

func readPolicy(path string) (claudePolicy, bool, error) {
	raw, err := readFollowingSymlinks(path)
	if os.IsNotExist(err) {
		return claudePolicy{}, false, nil
	}
	if err != nil {
		return claudePolicy{}, false, err
	}
	var p claudePolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, true, fmt.Errorf("cannot parse policy file %s", path)
	}
	return p, true, nil
}

func CheckClaudePolicy(paths []string, managed string, command Command) error {
	if _, err := os.Lstat(filepath.Join(managed, "managed-mcp.json")); err == nil {
		return errors.New("Claude managed MCP configuration has exclusive control; ask your administrator to register ohya")
	} else if !os.IsNotExist(err) {
		return err
	}
	administrator, _, err := readPolicy(filepath.Join(managed, "managed-settings.json"))
	if err != nil {
		return err
	}
	policies := []claudePolicy{administrator}
	for _, path := range paths {
		p, _, err := readPolicy(path)
		if err != nil {
			return err
		}
		policies = append(policies, p)
	}
	var allowed []mcpRule
	restricted := false
	for _, p := range policies {
		for _, r := range p.Denied {
			if ruleMatches(r, command) {
				return errors.New("Claude policy denies ohya; preserve that policy and review it with its owner")
			}
		}
		if !administrator.ManagedOnly && p.Allowed != nil {
			restricted = true
			allowed = append(allowed, p.Allowed...)
		}
	}
	if administrator.ManagedOnly {
		restricted = true
		allowed = administrator.Allowed
	}
	if !restricted {
		return nil
	}
	commandRestricted := false
	for _, r := range allowed {
		if len(r.Command) > 0 {
			commandRestricted = true
		}
	}
	for _, r := range allowed {
		if len(r.Command) > 0 && reflect.DeepEqual(r.Command, append([]string{command.Command}, command.Args...)) {
			return nil
		}
		if !commandRestricted && r.Name == "ohya" {
			return nil
		}
	}
	return errors.New("Claude policy does not allow the ohya MCP command; preserve it and ask its owner to allow ohya")
}

func PreflightClient(o ClientOptions, c Client, cwd string) error {
	var ancestors []string
	if cwd != "" {
		if !filepath.IsAbs(cwd) {
			return errors.New("setup working directory must be absolute")
		}
		for dir := cwd; ; dir = filepath.Dir(dir) {
			ancestors = append(ancestors, dir)
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	for _, dir := range ancestors {
		var path string
		switch c.Name {
		case "claude":
			path = filepath.Join(dir, ".mcp.json")
		case "codex":
			path = filepath.Join(dir, ".codex", "config.toml")
		case "pi":
			path = filepath.Join(dir, ".pi", "mcp.json")
		case "agy":
			path = filepath.Join(dir, ".agents", "mcp_config.json")
		}
		if path == c.Config {
			continue
		}
		raw, err := readFollowingSymlinks(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		exists := false
		if c.Name == "codex" {
			exists, err = tomlHasOhya(raw)
		} else {
			var root map[string]json.RawMessage
			root, err = jsonObject(raw)
			if err == nil {
				if servers, ok := root["mcpServers"]; ok {
					var entries map[string]json.RawMessage
					entries, err = jsonObject(servers)
					_, exists = entries["ohya"]
				}
			}
		}
		if err != nil {
			return fmt.Errorf("cannot inspect project MCP configuration %s", path)
		}
		if exists {
			return fmt.Errorf("%s defines ohya for this project; review that override before configuring the user entry", path)
		}
	}
	if c.Name == "claude" {
		configDir := filepath.Join(o.Home, ".claude")
		getenv := o.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		if override := getenv("CLAUDE_CONFIG_DIR"); override != "" {
			configDir = override
		}
		paths := []string{filepath.Join(configDir, "settings.json")}
		for _, dir := range ancestors {
			paths = append(paths, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json"))
		}
		managed := "/etc/claude-code"
		if runtime.GOOS == "darwin" {
			managed = "/Library/Application Support/ClaudeCode"
		}
		return CheckClaudePolicy(paths, managed, Command{Command: o.Binary, Args: o.Args})
	}
	return nil
}
