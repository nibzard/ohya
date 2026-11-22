package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/nibzard/ohya/internal/durablefile"
)

type JSONEntry struct {
	Path   string          `json:"path"`
	Parent string          `json:"parent"`
	Name   string          `json:"name"`
	Value  json.RawMessage `json:"value"`
}

type entryReceipt struct {
	Entry    JSONEntry       `json:"entry"`
	Previous json.RawMessage `json:"previous,omitempty"`
}

func jsonObject(raw []byte) (map[string]json.RawMessage, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("expected a JSON object")
	}
	return value, nil
}

func sameJSON(a, b json.RawMessage) bool {
	var left, right bytes.Buffer
	if json.Compact(&left, a) != nil || json.Compact(&right, b) != nil {
		return false
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}

func readRegular(path string) ([]byte, error) {
	return readBounded(path, false)
}

func readFollowingSymlinks(path string) ([]byte, error) {
	return readBounded(path, true)
}

func readBounded(path string, followSymlinks bool) ([]byte, error) {
	stat, flags := os.Lstat, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK
	if followSymlinks {
		stat, flags = os.Stat, os.O_RDONLY|syscall.O_NONBLOCK
	}
	info, err := stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	const limit = 8 << 20
	if opened.Size() > limit {
		return nil, fmt.Errorf("%s exceeds the 8 MiB setup file bound", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s exceeds the 8 MiB setup file bound", path)
	}
	return data, nil
}

func readEntry(e JSONEntry) (map[string]json.RawMessage, map[string]json.RawMessage, []byte, error) {
	raw, err := readRegular(e.Path)
	if errors.Is(err, os.ErrNotExist) {
		raw, err = []byte("{}"), nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	parsed := raw
	if len(bytes.TrimSpace(parsed)) == 0 {
		parsed = []byte("{}")
	}
	root, err := jsonObject(parsed)
	if err != nil {
		return nil, nil, nil, err
	}
	children := map[string]json.RawMessage{}
	if value, ok := root[e.Parent]; ok {
		children, err = jsonObject(value)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return root, children, raw, nil
}

func writeEntry(e JSONEntry, root, children map[string]json.RawMessage, original []byte) error {
	encoded, err := json.Marshal(children)
	if err != nil {
		return err
	}
	root[e.Parent] = encoded
	output, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	current, err := readRegular(e.Path)
	if errors.Is(err, os.ErrNotExist) {
		current, err = []byte("{}"), nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return fmt.Errorf("%s changed during setup; rerun setup", e.Path)
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o700); err != nil {
		return err
	}
	return durablefile.Write(e.Path, append(output, '\n'))
}

func ApplyJSONEntry(e JSONEntry, receiptPath string) (bool, error) {
	if e.Parent == "" || e.Name == "" || !json.Valid(e.Value) {
		return false, errors.New("invalid setup entry")
	}
	root, children, original, err := readEntry(e)
	if err != nil {
		return false, err
	}
	receipt := entryReceipt{}
	raw, err := readRegular(receiptPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return false, err
		}
		if receipt.Entry.Path != e.Path || receipt.Entry.Parent != e.Parent || receipt.Entry.Name != e.Name {
			return false, errors.New("setup receipt belongs to another entry")
		}
		if current, exists := children[e.Name]; exists && !sameJSON(current, receipt.Entry.Value) && !sameJSON(current, receipt.Previous) {
			return false, fmt.Errorf("%s entry %s has been edited; preserve it", e.Path, e.Name)
		}
	case errors.Is(err, os.ErrNotExist):
		if _, exists := children[e.Name]; exists {
			return false, fmt.Errorf("%s already contains an unowned %s entry", e.Path, e.Name)
		}
	default:
		return false, err
	}
	if sameJSON(children[e.Name], e.Value) {
		return false, nil
	}
	if err := backupConfiguration(e.Path, receiptPath, original); err != nil {
		return false, err
	}
	receipt.Previous = children[e.Name]
	receipt.Entry = e
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o700); err != nil {
		return false, err
	}
	if err := durablefile.Write(receiptPath, encoded); err != nil {
		return false, err
	}
	children[e.Name] = e.Value
	if err := writeEntry(e, root, children, original); err != nil {
		return false, err
	}
	return true, nil
}

func RemoveJSONEntry(receiptPath string) (bool, error) {
	raw, err := readRegular(receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var receipt entryReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return false, err
	}
	e := receipt.Entry
	root, children, original, err := readEntry(e)
	if err != nil {
		return false, err
	}
	if current, exists := children[e.Name]; exists {
		if !sameJSON(current, e.Value) && !sameJSON(current, receipt.Previous) {
			return false, fmt.Errorf("%s entry %s has been edited; preserve it", e.Path, e.Name)
		}
		if err := backupConfiguration(e.Path, receiptPath, original); err != nil {
			return false, err
		}
		delete(children, e.Name)
		if err := writeEntry(e, root, children, original); err != nil {
			return false, err
		}
	}
	if err := os.Remove(receiptPath); err != nil {
		return false, err
	}
	return true, nil
}
