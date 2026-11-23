package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nibzard/ohya/internal/buildinfo"
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
	dir, version, dry, setupArgs, err := parseInstallArgs(args, filepath.Join(home, ".local", "bin"))
	if err != nil {
		return err
	}
	if version != "" {
		current, _, _, _ := buildinfo.Identity()
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
		return fmt.Errorf("integrated setup did not complete: %w; the setup output above names the next step", err)
	}
	return nil
}

func parseInstallArgs(args []string, defaultDir string) (dir, version string, dry bool, setupArgs []string, err error) {
	dir = defaultDir
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--bin-dir" || arg == "--version":
			if i+1 >= len(args) {
				return "", "", false, nil, usageError{cmd: "ohya install", msg: arg + " needs a value"}
			}
			i++
			if arg == "--bin-dir" {
				dir = args[i]
			} else {
				version = args[i]
			}
		case strings.HasPrefix(arg, "--bin-dir="):
			dir = strings.TrimPrefix(arg, "--bin-dir=")
		case strings.HasPrefix(arg, "--version="):
			version = strings.TrimPrefix(arg, "--version=")
		default:
			if arg == "--dry-run" || arg == "--dry-run=true" || arg == "--dry-run=1" {
				dry = true
			} else if arg == "--dry-run=false" || arg == "--dry-run=0" {
				dry = false
			}
			setupArgs = append(setupArgs, arg)
		}
	}
	return dir, version, dry, setupArgs, nil
}
