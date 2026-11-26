package device

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nibzard/ohya/internal/durablefile"
)

const FileName = "device.json"

const stateVersion = 1

const idBytes = 16

var ErrUnusable = errors.New("device: identity file exists but cannot be used")

type identityFile struct {
	Version  int    `json:"version"`
	DeviceID string `json:"device_id"`
}

func Load(dir string) (string, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return parse(path, data)
}

func LoadOrCreate(dir string) (string, error) {
	path := filepath.Join(dir, FileName)
	if data, err := os.ReadFile(path); err == nil {
		return parse(path, data)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("device: read %s: %w", path, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("device: create state directory %s: %w", dir, err)
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	data, err := marshal(id)
	if err != nil {
		return "", err
	}
	if err := durablefile.Write(path, data); err != nil {
		return "", err
	}
	return id, nil
}

func newID() (string, error) {
	var b [idBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("device: generate ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func marshal(id string) ([]byte, error) {
	data, err := json.MarshalIndent(identityFile{Version: stateVersion, DeviceID: id}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("device: encode identity: %w", err)
	}
	return append(data, '\n'), nil
}

func parse(path string, data []byte) (string, error) {
	var file identityFile
	if err := json.Unmarshal(data, &file); err != nil {
		return "", fmt.Errorf("%w: parse %s: %v", ErrUnusable, path, err)
	}
	if file.Version != stateVersion {
		return "", fmt.Errorf("%w: %s has version %d, want %d", ErrUnusable, path, file.Version, stateVersion)
	}
	if !validID(file.DeviceID) {
		return "", fmt.Errorf("%w: %s holds no valid device ID", ErrUnusable, path)
	}
	return file.DeviceID, nil
}

func validID(id string) bool {
	if len(id) != 2*idBytes {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
