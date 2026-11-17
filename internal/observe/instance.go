package observe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func validInstanceName(name string) bool {
	if len(name) != 32 {
		return false
	}
	for _, c := range name {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func EnsureInstanceDir(dir, instanceID string) (string, error) {
	if !validInstanceName(instanceID) {
		return "", fmt.Errorf("observe: instance id %q is not a valid directory name", instanceID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("observe: create observe directory: %w", err)
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("observe: observe directory %s is not a directory", dir)
	}
	instance := filepath.Join(dir, instanceID)
	if err := os.Mkdir(instance, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("observe: create instance directory: %w", err)
		}
		if info, lerr := os.Lstat(instance); lerr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("observe: instance path %s exists and is not a usable directory", instance)
		}
	}
	if err := os.Chmod(instance, 0o700); err != nil {
		return "", fmt.Errorf("observe: restrict instance directory: %w", err)
	}
	return instance, nil
}
