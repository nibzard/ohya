package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/nibzard/ohya/internal/durablefile"
)

type Receipt struct {
	Digest   string `json:"digest"`
	Previous string `json:"previous,omitempty"`
	Recovery string `json:"recovery,omitempty"`
}

type Result struct {
	Path     string `json:"path"`
	Recovery string `json:"recovery,omitempty"`
	Changed  bool   `json:"changed"`
}

func digest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

func Binary(ctx context.Context, source, dir string) (Result, error) {
	if !filepath.IsAbs(source) || !filepath.IsAbs(dir) {
		return Result{}, errors.New("installation paths must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Result{}, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".ohya-install.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Result{}, errors.New("another installer owns this destination")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	next, err := digest(source)
	if err != nil {
		return Result{}, err
	}
	path := filepath.Join(dir, "ohya")
	result := Result{Path: path}
	current, err := digest(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	receiptPath := filepath.Join(dir, ".ohya-install.json")
	var receipt Receipt
	info, err := os.Lstat(receiptPath)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return result, errors.New("installer receipt is not a regular file")
		}
		raw, err := os.ReadFile(receiptPath)
		if err != nil {
			return result, err
		}
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return result, err
		}
		if exists && current != receipt.Digest && current != receipt.Previous {
			return result, errors.New("installed executable has changed; preserve it")
		}
	case os.IsNotExist(err):
		if exists {
			return result, errors.New("destination contains an executable not owned by this installer")
		}
	default:
		return result, err
	}
	if current == next {
		return result, nil
	}
	if exists {
		recoveryDir := filepath.Join(dir, ".ohya-backups")
		if err := os.MkdirAll(recoveryDir, 0700); err != nil {
			return result, err
		}
		backup, err := os.MkdirTemp(recoveryDir, "install-")
		if err != nil {
			return result, err
		}
		result.Recovery = filepath.Join(backup, "ohya")
		if err := copyFile(path, result.Recovery, 0700); err != nil {
			return result, err
		}
	}
	staging, err := os.MkdirTemp(dir, ".ohya-stage-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(staging)
	staged := filepath.Join(staging, "ohya")
	if err := copyFile(source, staged, 0755); err != nil {
		return result, err
	}
	stagedDigest, err := digest(staged)
	if err != nil {
		return result, err
	}
	if stagedDigest != next {
		return result, errors.New("source executable changed during installation")
	}
	latest, err := digest(path)
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	if latest != current {
		return result, errors.New("destination changed during installation")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	raw, err := json.Marshal(Receipt{Digest: next, Previous: current, Recovery: result.Recovery})
	if err != nil {
		return result, err
	}
	if err := durablefile.Write(receiptPath, raw); err != nil {
		return result, err
	}
	if err := os.Rename(staged, path); err != nil {
		return result, err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return result, err
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return result, err
	}
	result.Changed = true
	return result, nil
}
