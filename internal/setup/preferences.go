package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Preferences struct {
	Flags map[string]string `json:"flags"`
	Homes map[string]string `json:"homes"`
}

func (p Preferences) Validate() error {
	for name, value := range p.Flags {
		switch name {
		case "agents":
			if value == "" {
				return fmt.Errorf("saved agent selection is empty")
			}
		case "no-agents", "no-service", "no-modify-path":
			if value != "true" && value != "false" {
				return fmt.Errorf("invalid saved flag %s", name)
			}
		case "claude-root", "codex-root", "pi-root":
			if value != "" && !filepath.IsAbs(value) {
				return fmt.Errorf("saved %s must be absolute", name)
			}
		case "exclude":
			if value == "" {
				continue
			}
			for _, glob := range strings.Split(value, "\n") {
				if err := validSavedExcludeGlob(glob); err != nil {
					return fmt.Errorf("saved %s: %w", name, err)
				}
			}
		default:
			return fmt.Errorf("unknown saved setup flag %s", name)
		}
	}
	for name, path := range p.Homes {
		switch name {
		case "CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR":
		default:
			return fmt.Errorf("unknown saved client home %s", name)
		}
		if path != "" && !filepath.IsAbs(path) {
			return fmt.Errorf("saved %s must be absolute", name)
		}
	}
	return nil
}

func validSavedExcludeGlob(glob string) error {
	switch {
	case glob == "":
		return fmt.Errorf("glob is empty")
	case strings.ContainsAny(glob, "\r\x00"):
		return fmt.Errorf("glob %q contains a control character", glob)
	}
	if _, err := filepath.Match(glob, ""); err != nil {
		return fmt.Errorf("glob %q: %w", glob, err)
	}
	return nil
}

func LoadPreferences(state string) (Preferences, error) {
	raw, err := readRegular(filepath.Join(state, "setup", "preferences.json"))
	if os.IsNotExist(err) {
		return Preferences{}, nil
	}
	if err != nil {
		return Preferences{}, err
	}
	var p Preferences
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

func SavePreferences(state string, p Preferences) error {
	if err := p.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = ApplyFile(filepath.Join(state, "setup", "preferences.json"), filepath.Join(state, "setup", "preferences-receipt.json"), raw)
	return err
}
