package device

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nibzard/ohya/internal/durablefile"
)

const HubAddressFileName = "hub-address.json"

const hubAddressVersion = 1

const maxHubAddressBytes = 8192

type hubAddressFile struct {
	Version int    `json:"version"`
	Address string `json:"address"`
}

var ErrHubAddressUnusable = errors.New("device: hub address file exists but cannot be used; run ohya join with a fresh ohya1 code to save the hub's address again")

func SaveHubAddress(dir, addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("device: save hub address: the address is empty")
	}
	if len(addr) > maxHubAddressBytes {
		return fmt.Errorf("device: save hub address: length %d is above %d", len(addr), maxHubAddressBytes)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("device: create state directory %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(hubAddressFile{Version: hubAddressVersion, Address: addr}, "", "  ")
	if err != nil {
		return fmt.Errorf("device: encode hub address: %w", err)
	}
	return durablefile.Write(filepath.Join(dir, HubAddressFileName), append(data, '\n'))
}

func LoadHubAddress(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, HubAddressFileName))
	if err != nil {
		return "", err
	}
	var file hubAddressFile
	if err := json.Unmarshal(data, &file); err != nil {
		return "", fmt.Errorf("%w: parse: %v", ErrHubAddressUnusable, err)
	}
	if file.Version != hubAddressVersion {
		return "", fmt.Errorf("%w: version %d, want %d", ErrHubAddressUnusable, file.Version, hubAddressVersion)
	}
	addr := strings.TrimSpace(file.Address)
	if addr == "" {
		return "", fmt.Errorf("%w: holds no hub address", ErrHubAddressUnusable)
	}
	if len(addr) > maxHubAddressBytes {
		return "", fmt.Errorf("%w: address length %d is above %d", ErrHubAddressUnusable, len(addr), maxHubAddressBytes)
	}
	return addr, nil
}
