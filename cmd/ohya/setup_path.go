package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/nibzard/ohya/internal/setup"
)

func setupPathConflict(binary string) *setup.Result {
	current, err := exec.LookPath("ohya")
	if err != nil {
		return nil
	}
	current, err = filepath.Abs(current)
	if err != nil || current == binary {
		return nil
	}
	installed, installedErr := os.Stat(binary)
	found, foundErr := os.Stat(current)
	if installedErr == nil && foundErr == nil && os.SameFile(installed, found) {
		return nil
	}
	return &setup.Result{Component: "path-command", Status: "warning", Detail: fmt.Sprintf("PATH currently selects %s. Use %s or update PATH.", current, binary)}
}
