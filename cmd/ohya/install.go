package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"

	"github.com/nibzard/ohya/internal/buildinfo"
	"github.com/nibzard/ohya/internal/install"
)

const installUsage = "Usage: ohya install [--version VERSION] [--bin-dir DIRECTORY] [setup flags]"

func runInstallCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && isHelpArg(args[0]) {
		fmt.Fprintln(stdout, installUsage)
		return nil
	}
	home, source, err := setupPaths()
	if err != nil {
		return err
	}
	dir, version, dry, setupArgs, err := parseInstallArgs(args, filepath.Join(home, ".local", "bin"))
	if errors.Is(err, errHelp) {
		fmt.Fprintln(stdout, installUsage)
		return nil
	}
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
	c, flags := newSetupFlags("", "")
	c.spec.name = "ohya install"
	binDir := c.fs.String("bin-dir", defaultDir, "executable installation directory")
	release := c.fs.String("version", "", "required release version")
	if err := c.parse(args, io.Discard); err != nil {
		return "", "", false, nil, err
	}
	if c.fs.NArg() != 0 {
		return "", "", false, nil, c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	for i := 0; i < len(args); {
		arg := args[i]
		if arg == "--" {
			setupArgs = append(setupArgs, arg)
			break
		}
		name, takesNext := commandFlag(c.fs, arg)
		next := i + 1
		if takesNext {
			next++
		}
		if name != "bin-dir" && name != "version" {
			setupArgs = append(setupArgs, args[i:next]...)
		}
		i = next
	}
	return *binDir, *release, *flags.dry, setupArgs, nil
}
