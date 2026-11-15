package setup

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Service struct {
	Path    string
	Content []byte
	Start   [][]string
	Stop    [][]string
	Loaded  []string
	Check   []string
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "$", "$$")
	return strconv.Quote(value)
}

func ServicePlan(platform, home, binary string, args []string) (Service, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(binary) {
		return Service{}, fmt.Errorf("service paths must be absolute")
	}
	for _, s := range append([]string{home, binary}, args...) {
		if strings.ContainsAny(s, "\n\r\x00") {
			return Service{}, fmt.Errorf("service arguments contain control characters")
		}
	}
	switch platform {
	case "linux":
		words := []string{systemdQuote(binary)}
		for _, arg := range args {
			words = append(words, systemdQuote(arg))
		}
		content := "[Unit]\nDescription=ohya agent memory\nStartLimitIntervalSec=60\nStartLimitBurst=5\n\n[Service]\nType=simple\nExecStart=" + strings.Join(words, " ") + "\nWorkingDirectory=" + strconv.Quote(strings.ReplaceAll(home, "%", "%%")) + "\nRestart=on-failure\nRestartSec=5\nUMask=0077\nStandardOutput=journal\nStandardError=journal\nLogRateLimitIntervalSec=60\nLogRateLimitBurst=20\n\n[Install]\nWantedBy=default.target\n"
		return Service{Path: filepath.Join(home, ".config", "systemd", "user", "ohya.service"), Content: []byte(content), Start: [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "ohya.service"}, {"systemctl", "--user", "restart", "ohya.service"}}, Stop: [][]string{{"systemctl", "--user", "disable", "--now", "ohya.service"}}, Loaded: []string{"systemctl", "--user", "show", "--property=LoadState", "--value", "ohya.service"}, Check: []string{"systemctl", "--user", "show", "--property=ActiveState,SubState,MainPID", "ohya.service"}}, nil
	case "darwin":
		var b strings.Builder
		b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict><key>Label</key><string>com.nibzard.ohya</string><key>ProgramArguments</key><array>")
		for _, arg := range append([]string{binary}, args...) {
			b.WriteString("<string>")
			if err := xml.EscapeText(&b, []byte(arg)); err != nil {
				return Service{}, err
			}
			b.WriteString("</string>")
		}
		b.WriteString("</array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>5</integer><key>Umask</key><integer>63</integer></dict></plist>\n")
		path := filepath.Join(home, "Library", "LaunchAgents", "com.nibzard.ohya.plist")
		domain := "gui/" + strconv.Itoa(os.Getuid())
		return Service{Path: path, Content: []byte(b.String()), Start: [][]string{{"launchctl", "bootstrap", domain, path}, {"launchctl", "kickstart", "-k", domain + "/com.nibzard.ohya"}}, Stop: [][]string{{"launchctl", "bootout", domain + "/com.nibzard.ohya"}}, Loaded: []string{"launchctl", "print", domain + "/com.nibzard.ohya"}, Check: []string{"launchctl", "print", domain + "/com.nibzard.ohya"}}, nil
	default:
		return Service{}, fmt.Errorf("user services are not supported on %s", platform)
	}
}

func serviceOutput(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("empty service command")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = os.TempDir()
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w; check your user service manager", strings.Join(args, " "), err)
	}
	return output.String(), nil
}

func serviceRunning(platform, output string) error {
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	state, pid := values["ActiveState"], values["MainPID"]
	if platform == "darwin" {
		state, pid = values["state"], values["pid"]
	}
	process, err := strconv.Atoi(pid)
	if err != nil || process <= 0 {
		return errors.New("memory service has no running process")
	}
	if platform == "darwin" {
		if state != "running" {
			return errors.New("memory LaunchAgent is loaded but not running")
		}
	} else if state != "active" || values["SubState"] != "running" {
		return errors.New("memory service is not running")
	}
	return nil
}

func RunServiceLoaded(ctx context.Context, args []string) error {
	output, err := serviceOutput(ctx, args)
	if err != nil {
		return err
	}
	if args[0] == "systemctl" && strings.TrimSpace(output) != "loaded" {
		return errors.New("memory service is not loaded")
	}
	return nil
}

func RunServiceCommand(ctx context.Context, args []string) error {
	output, err := serviceOutput(ctx, args)
	if err != nil {
		return err
	}
	if args[0] == "launchctl" && len(args) > 1 && args[1] == "print" {
		return serviceRunning("darwin", output)
	}
	if args[0] == "systemctl" && strings.Contains(strings.Join(args, " "), "--property=ActiveState,SubState,MainPID") {
		return serviceRunning("linux", output)
	}
	return nil
}

func waitService(ctx context.Context, s Service, run func(context.Context, []string) error) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	successes := 0
	var last error
	for {
		last = run(ctx, s.Check)
		if last == nil {
			successes++
		} else {
			successes = 0
		}
		if successes == 2 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service did not stay ready: %w", errors.Join(ctx.Err(), last))
		case <-time.After(250 * time.Millisecond):
		}
	}
}
