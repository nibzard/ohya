// The device-side pairing credential: the secret the hub mints when the
// device consumes a pairing invitation. It lives in its own protected state
// file, next to the identity file, and authenticates every hub connection.

package device

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nibzard/ohya/internal/durablefile"
)

// CredentialFileName is the state file holding the device's hub credential.
const CredentialFileName = "credential.json"

// credentialVersion is the credential file format version.
const credentialVersion = 1

// maxSecretLen bounds the stored secret; it matches the wire bound of
// internal/hubsync.
const maxSecretLen = 128

// ErrCredentialUnusable reports that a credential file exists but cannot be
// used. Never delete it to re-pair: the hub registry holds the device
// identity as paired, so a fresh invitation is refused. Restore the file, or
// have the hub revoke this identity and start over with a new one.
var ErrCredentialUnusable = errors.New("device: credential file exists but cannot be used; restore it, or ask the hub to revoke this device identity and re-pair with a new one")

// Credential is the device's pairing credential: the secret the hub minted
// when this device consumed its pairing invitation. It is not the device
// identity; it authenticates the identity.
type Credential struct {
	DeviceID string
	Secret   string
}

// credentialFile is the on-disk shape of CredentialFileName.
type credentialFile struct {
	Version  int    `json:"version"`
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

// SaveCredential durably writes the credential to dir with owner-only
// permissions. The device must have its identity first: the credential
// authenticates an identity, it does not create one.
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

// LoadCredential reads the credential from dir. A missing file is
// fs.ErrNotExist: the device has not paired yet. An existing but unusable
// file is ErrCredentialUnusable, never a silent re-pair.
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
