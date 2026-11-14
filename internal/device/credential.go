package device

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nibzard/ohya/internal/durablefile"
)

const CredentialFileName = "credential.json"

const credentialVersion = 1

const maxSecretLen = 128

var ErrCredentialUnusable = errors.New("device: credential file exists but cannot be used; restore it, or ask the hub to revoke this device identity and re-pair with a new one")

type Credential struct {
	DeviceID string
	Secret   string
}

type credentialFile struct {
	Version  int    `json:"version"`
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

func SaveCredential(dir string, c Credential) error {
	if !validID(c.DeviceID) {
		return fmt.Errorf("device: save credential: device ID %q is not a valid identity", c.DeviceID)
	}
	if c.Secret == "" || len(c.Secret) > maxSecretLen {
		return fmt.Errorf("device: save credential: secret length %d is empty or above %d", len(c.Secret), maxSecretLen)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("device: create state directory %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(credentialFile{
		Version:  credentialVersion,
		DeviceID: c.DeviceID,
		Secret:   c.Secret,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("device: encode credential: %w", err)
	}
	return durablefile.Write(filepath.Join(dir, CredentialFileName), append(data, '\n'))
}

func LoadCredential(dir string) (Credential, error) {
	path := filepath.Join(dir, CredentialFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, err
	}
	var file credentialFile
	if err := json.Unmarshal(data, &file); err != nil {
		return Credential{}, fmt.Errorf("%w: parse %s: %v", ErrCredentialUnusable, path, err)
	}
	if file.Version != credentialVersion {
		return Credential{}, fmt.Errorf("%w: %s has version %d, want %d",
			ErrCredentialUnusable, path, file.Version, credentialVersion)
	}
	if !validID(file.DeviceID) || file.Secret == "" || len(file.Secret) > maxSecretLen {
		return Credential{}, fmt.Errorf("%w: %s holds no valid credential", ErrCredentialUnusable, path)
	}
	return Credential{DeviceID: file.DeviceID, Secret: file.Secret}, nil
}
