package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/nibzard/ohya/internal/durablefile"
)

type fileReceipt struct {
	WholeFile bool   `json:"whole_file"`
	Path      string `json:"path"`
	Content   []byte `json:"content"`
	Previous  []byte `json:"previous,omitempty"`
}

func Lock(state string) (func(), error) {
	dir := filepath.Join(state, "setup")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "setup.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another setup process owns this state directory")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func loadFileReceipt(path string) (fileReceipt, error) {
	raw, err := readRegular(path)
	if err != nil {
		return fileReceipt{}, err
	}
	var r fileReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return fileReceipt{}, err
	}
	if !filepath.IsAbs(r.Path) || len(r.Content) == 0 {
		return fileReceipt{}, errors.New("invalid setup file receipt")
	}
	return r, nil
}

func ApplyFile(path, receiptPath string, content []byte) (bool, error) {
	if !filepath.IsAbs(path) || len(content) == 0 {
		return false, errors.New("invalid setup file")
	}
	current, err := readRegular(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	r, err := loadFileReceipt(receiptPath)
	switch {
	case err == nil:
		if r.Path != path {
			return false, errors.New("setup receipt belongs to another file")
		}
		if exists && !bytes.Equal(current, r.Content) && !bytes.Equal(current, r.Previous) {
			return false, fmt.Errorf("%s has been edited; preserve it", path)
		}
	case errors.Is(err, os.ErrNotExist):
		if exists {
			return false, fmt.Errorf("%s is not owned by setup", path)
		}
	default:
		return false, err
	}
	if bytes.Equal(current, content) {
		return false, nil
	}
	r = fileReceipt{WholeFile: true, Path: path, Content: content, Previous: current}
	raw, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0700); err != nil {
		return false, err
	}
	if err := durablefile.Write(receiptPath, raw); err != nil {
		return false, err
	}
	latest, err := readRegular(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if !bytes.Equal(latest, current) {
		return false, fmt.Errorf("%s changed during setup", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	return true, durablefile.Write(path, content)
}

func RemoveFile(receiptPath string) (bool, error) {
	r, err := loadFileReceipt(receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current, err := readRegular(r.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err == nil {
		if !bytes.Equal(current, r.Content) && !bytes.Equal(current, r.Previous) {
			return false, fmt.Errorf("%s has been edited; preserve it", r.Path)
		}
		if err := os.Remove(r.Path); err != nil {
			return false, err
		}
	}
	return true, os.Remove(receiptPath)
}
