package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"

	"github.com/nibzard/ohya/internal/install"
)

func runInstallCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && isHelpArg(args[0]) {
		fmt.Fprintln(stdout, "Usage: ohya install [--version VERSION] [--bin-dir DIRECTORY] [setup flags]")
		return nil
	}
	home, source, err := setupPaths()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".local", "bin")
	version := ""
	var setupArgs []string
	dry := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--bin-dir", "--version":
			if i+1 >= len(args) {
				return usageError{cmd: "ohya install", msg: args[i] + " needs a value"}
			}
			key := args[i]
			i++
			if key == "--bin-dir" {
				dir = args[i]
			} else {
				version = args[i]
			}
		default:
			if args[i] == "--dry-run" {
				dry = true
			}
			setupArgs = append(setupArgs, args[i])
		}
	}
	if version != "" {
		current, _, _, _ := buildIdentity()
		if current != version {
			return fmt.Errorf("installer release identity %s differs from requested %s", current, version)
		}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, "ohya")
	if !dry {
		result, err := install.Binary(context.Background(), source, dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "ohya: executable installed at %s\n", result.Path)
		if result.Recovery != "" {
			fmt.Fprintf(stderr, "ohya: previous executable saved at %s\n", result.Recovery)
		}
	}
	setupArgs = append([]string{"setup", "--binary", target}, setupArgs...)
	executable := target
	if dry {
		executable = source
	}
	cmd := exec.Command(executable, setupArgs...)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("integrated setup did not complete: %w; rerun %s setup", err, target)
	}
	return nil
}
