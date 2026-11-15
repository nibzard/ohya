package setup

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nibzard/ohya/internal/durablefile"
)

func backupConfiguration(path, receiptPath string, original []byte) error {
	current, err := readRegular(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return fmt.Errorf("%s changed before backup; rerun setup", path)
	}
	dir := receiptPath + ".backups"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	name := filepath.Join(dir, fmt.Sprintf("%x", sha256.Sum256(current)))
	saved, err := readRegular(name)
	if err == nil {
		if !bytes.Equal(saved, current) {
			return fmt.Errorf("configuration backup %s has been edited", name)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return durablefile.Write(name, current)
}
