package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/nibzard/ohya/internal/durablefile"
	"github.com/pelletier/go-toml/v2"
)

type Command struct {
	Command string   `json:"command" toml:"command"`
	Args    []string `json:"args" toml:"args"`
}

type tomlReceipt struct {
	Path     string `json:"path"`
	Block    string `json:"block"`
	Previous string `json:"previous,omitempty"`
}

func tomlHasOhya(raw []byte) (bool, error) {
	var config struct {
		Servers map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(raw, &config); err != nil {
		return false, err
	}
	_, exists := config.Servers["ohya"]
	return exists, nil
}

func loadTOMLReceipt(path string) (tomlReceipt, error) {
	raw, err := readRegular(path)
	if err != nil {
		return tomlReceipt{}, err
	}
	var r tomlReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return tomlReceipt{}, err
	}
	if r.Path == "" || r.Block == "" {
		return tomlReceipt{}, errors.New("invalid TOML setup receipt")
	}
	return r, nil
}

func tomlContent(path string) ([]byte, error) {
	raw, err := readRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

func replaceTOML(path string, original, updated []byte) error {
	current, err := tomlContent(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return fmt.Errorf("%s changed during setup; rerun setup", path)
	}
	if _, err := tomlHasOhya(updated); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return durablefile.Write(path, updated)
}

func ownedTOML(raw []byte, r tomlReceipt) ([]byte, error) {
	var current struct {
		Servers map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(raw, &current); err != nil {
		return nil, err
	}
	actual, exists := current.Servers["ohya"]
	if !exists {
		return raw, nil
	}
	for _, block := range []string{r.Block, r.Previous} {
		if block == "" {
			continue
		}
		var expected struct {
			Servers map[string]any `toml:"mcp_servers"`
		}
		if err := toml.Unmarshal([]byte(block), &expected); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(actual, expected.Servers["ohya"]) {
			continue
		}
		if bytes.Count(raw, []byte(block)) == 1 {
			return removeBlock(raw, []byte(block)), nil
		}
		if updated, ok := removeOhyaTable(raw); ok {
			return updated, nil
		}
	}
	return nil, errors.New("Codex ohya entry has been edited; preserve it")
}

func removeOhyaTable(raw []byte) ([]byte, bool) {
	start, end := -1, len(raw)
	offset := 0
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("[")) {
			if start >= 0 && end == len(raw) {
				end = offset
			}
			if isOhyaTableHeader(trimmed) {
				if start >= 0 {
					return nil, false
				}
				start = offset
			}
		}
		offset += len(line)
	}
	if start < 0 {
		return nil, false
	}
	updated := append(bytes.Clone(raw[:start]), raw[end:]...)
	return updated, sameConfigWithoutOhya(raw, updated)
}

func isOhyaTableHeader(line []byte) bool {
	if bytes.HasPrefix(line, []byte("[[")) {
		return false
	}
	var header struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(line, &header); err != nil {
		return false
	}
	table, exists := header.Servers["ohya"]
	return exists && len(table) == 0 && len(header.Servers) == 1
}

func sameConfigWithoutOhya(raw, updated []byte) bool {
	var before, after map[string]any
	if toml.Unmarshal(raw, &before) != nil || toml.Unmarshal(updated, &after) != nil {
		return false
	}
	servers, ok := before["mcp_servers"].(map[string]any)
	if !ok {
		return false
	}
	delete(servers, "ohya")
	if len(servers) == 0 {
		delete(before, "mcp_servers")
	}
	return reflect.DeepEqual(before, after)
}

func ApplyTOMLEntry(path, receiptPath string, command Command) (bool, error) {
	if !filepath.IsAbs(path) || command.Command == "" {
		return false, errors.New("invalid Codex setup entry")
	}
	raw, err := tomlContent(path)
	if err != nil {
		return false, err
	}
	exists, err := tomlHasOhya(raw)
	if err != nil {
		return false, err
	}
	r, err := loadTOMLReceipt(receiptPath)
	base := raw
	switch {
	case err == nil:
		if r.Path != path {
			return false, errors.New("setup receipt belongs to another Codex configuration")
		}
		base, err = ownedTOML(raw, r)
		if err != nil {
			return false, err
		}
	case errors.Is(err, os.ErrNotExist):
		if exists {
			return false, errors.New("Codex already contains an unowned ohya entry")
		}
	default:
		return false, err
	}
	fields, err := toml.Marshal(command)
	if err != nil {
		return false, err
	}
	block := "\n[mcp_servers.ohya]\n" + string(fields)
	if bytes.Equal(raw, append(bytes.Clone(base), []byte(block)...)) {
		return false, nil
	}
	if err := backupConfiguration(path, receiptPath, raw); err != nil {
		return false, err
	}
	previous := r.Block
	if r.Previous != "" && strings.Contains(string(raw), r.Previous) {
		previous = r.Previous
	}
	r = tomlReceipt{Path: path, Block: block, Previous: previous}
	encoded, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0700); err != nil {
		return false, err
	}
	if err := durablefile.Write(receiptPath, encoded); err != nil {
		return false, err
	}
	return true, replaceTOML(path, raw, append(bytes.Clone(base), []byte(block)...))
}

func RemoveTOMLEntry(receiptPath string) (bool, error) {
	r, err := loadTOMLReceipt(receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := tomlContent(r.Path)
	if err != nil {
		return false, err
	}
	updated, err := ownedTOML(raw, r)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(raw, updated) {
		if err := backupConfiguration(r.Path, receiptPath, raw); err != nil {
			return false, err
		}
		if err := replaceTOML(r.Path, raw, updated); err != nil {
			return false, err
		}
	}
	return true, os.Remove(receiptPath)
}
