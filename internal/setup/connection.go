package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
)

type Connection struct {
	Mode       string `json:"mode"`
	ControlURL string `json:"control_url,omitempty"`
	HubAddress string `json:"hub_address,omitempty"`
}

func (c Connection) Validate() error {
	switch c.Mode {
	case "local":
		if c.ControlURL != "" || c.HubAddress != "" {
			return errors.New("local setup cannot specify a hub")
		}
	case "remote":
		u, err := url.Parse(c.ControlURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("coordination URL must be HTTP or HTTPS without credentials, query, or fragment")
		}
		if _, _, err := net.SplitHostPort(c.HubAddress); err != nil {
			return fmt.Errorf("invalid hub address: %w", err)
		}
	default:
		return errors.New("setup mode must be local or remote")
	}
	return nil
}

func LoadConnection(state string) (Connection, error) {
	raw, err := readRegular(filepath.Join(state, "setup", "connection.json"))
	if os.IsNotExist(err) {
		return Connection{Mode: "local"}, nil
	}
	if err != nil {
		return Connection{}, err
	}
	var c Connection
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func SaveConnection(state string, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = ApplyFile(filepath.Join(state, "setup", "connection.json"), filepath.Join(state, "setup", "connection-receipt.json"), raw)
	return err
}

func BindRemote(state string, c Connection) error {
	path := filepath.Join(state, "setup", "connection.json")
	if _, err := os.Lstat(path); err == nil {
		previous, err := LoadConnection(state)
		if err != nil {
			return err
		}
		if previous != c {
			return errors.New("this join state belongs to a different hub; use a new setup state directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return SaveConnection(state, c)
}
