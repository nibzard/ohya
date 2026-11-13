// Package device owns the local device identity: a stable random ID,
// persisted once and never regenerated in place. Authentication credentials
// are a separate later concern (see ARCHITECTURE.md).
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

// FileName is the state file holding the device identity. It lives in the
// local state directory.
const FileName = "device.json"

// stateVersion is the device state file format version.
const stateVersion = 1

// idBytes is the entropy of a device ID. Hex-encoded it is 32 characters.
const idBytes = 16

// ErrUnusable reports that a device identity file exists but cannot be used.
// Generating a replacement ID silently would fork the device identity and
// duplicate its sessions. Restore the file, or delete it to deliberately
// start over as a new device.
var ErrUnusable = errors.New("device: identity file exists but cannot be used; restore it, or delete it to start over as a new device")

// identityFile is the on-disk shape of FileName.
type identityFile struct {
	Version  int    `json:"version"`
	DeviceID string `json:"device_id"`
}

// LoadOrCreate returns the stable random device ID stored in dir, creating
// FileName on first use with a durable atomic write. The ID is 32 lowercase
// hex characters from crypto/rand and never changes once created.
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

// newID returns a fresh random device ID.
func newID() (string, error) {
	var b [idBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("device: generate ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// marshal encodes the identity file with a trailing newline.
func marshal(id string) ([]byte, error) {
	data, err := json.MarshalIndent(identityFile{Version: stateVersion, DeviceID: id}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("device: encode identity: %w", err)
	}
	return append(data, '\n'), nil
}

// parse validates an existing identity file and returns its device ID.
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

// validID reports whether id is a well-formed device ID.
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
