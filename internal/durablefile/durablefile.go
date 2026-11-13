// Package durablefile writes small local state files by atomic replacement
// with durable writes. A crash leaves either the previous file or the
// complete new one, never a torn or empty state file.
package durablefile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write atomically replaces the file at path with data. The bytes go to a
// temporary file in the same directory, are flushed to disk, and are renamed
// over the target; the directory entry is then flushed too, so the completed
// rename itself survives a crash. The target directory must already exist.
// The file is created with owner-only permissions.
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("durablefile: create temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("durablefile: write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("durablefile: flush %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("durablefile: close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("durablefile: replace %s: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

// syncDir flushes a directory entry so a completed rename cannot vanish
// after a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("durablefile: open directory %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("durablefile: flush directory %s: %w", dir, err)
	}
	return nil
}
