package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Connection struct {
	Mode string `json:"mode"`
}

func (c Connection) Validate() error {
	switch c.Mode {
	case "local", "remote":
		return nil
	default:
		return errors.New("setup mode must be local or remote")
	}
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
			return errors.New("this join state belongs to a different setup mode; use a new setup state directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return SaveConnection(state, c)
}
