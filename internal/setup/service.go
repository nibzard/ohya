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
	return planService(serviceTemplate{unit: "ohya.service", label: "com.nibzard.ohya", description: "ohya agent memory"}, platform, home, binary, args)
}

func HubServicePlan(platform, home, binary string, args []string) (Service, error) {
	return planService(serviceTemplate{unit: "ohya-serve.service", label: "com.nibzard.ohya.serve", description: "ohya memory hub for devices"}, platform, home, binary, args)
}

type serviceTemplate struct {
	unit        string
	label       string
	description string
}

func planService(t serviceTemplate, platform, home, binary string, args []string) (Service, error) {
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
		content := "[Unit]\nDescription=" + t.description + "\nStartLimitIntervalSec=60\nStartLimitBurst=5\n\n[Service]\nType=simple\nExecStart=" + strings.Join(words, " ") + "\nWorkingDirectory=" + strings.ReplaceAll(home, "%", "%%") + "\nRestart=on-failure\nRestartSec=5\nUMask=0077\nStandardOutput=journal\nStandardError=journal\nLogRateLimitIntervalSec=60\nLogRateLimitBurst=20\n\n[Install]\nWantedBy=default.target\n"
		return Service{Path: filepath.Join(home, ".config", "systemd", "user", t.unit), Content: []byte(content), Start: [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", t.unit}, {"systemctl", "--user", "restart", t.unit}}, Stop: [][]string{{"systemctl", "--user", "disable", "--now", t.unit}}, Loaded: []string{"systemctl", "--user", "show", "--property=LoadState", "--value", t.unit}, Check: []string{"systemctl", "--user", "show", "--property=ActiveState,SubState,MainPID", t.unit}}, nil
	case "darwin":
		var b strings.Builder
		b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict><key>Label</key><string>" + t.label + "</string><key>ProgramArguments</key><array>")
		for _, arg := range append([]string{binary}, args...) {
			b.WriteString("<string>")
			if err := xml.EscapeText(&b, []byte(arg)); err != nil {
				return Service{}, err
			}
			b.WriteString("</string>")
		}
		b.WriteString("</array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>5</integer><key>Umask</key><integer>63</integer></dict></plist>\n")
		path := filepath.Join(home, "Library", "LaunchAgents", t.label+".plist")
		domain := "gui/" + strconv.Itoa(os.Getuid())
		return Service{Path: path, Content: []byte(b.String()), Start: [][]string{{"launchctl", "bootstrap", domain, path}, {"launchctl", "kickstart", "-k", domain + "/" + t.label}}, Stop: [][]string{{"launchctl", "bootout", domain + "/" + t.label}}, Loaded: []string{"launchctl", "print", domain + "/" + t.label}, Check: []string{"launchctl", "print", domain + "/" + t.label}}, nil
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

func launchctlFields(output string) map[string]string {
	values := map[string]string{}
	depth := 0
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasSuffix(trimmed, "{"):
			depth++
		case trimmed == "}":
			depth--
		case depth == 1:
			if key, value, found := strings.Cut(trimmed, "="); found {
				values[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	}
	return values
}

func systemdFields(output string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		if key, value, found := strings.Cut(strings.TrimSpace(line), "="); found {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values
}

func serviceRunning(platform, output, name string) error {
	values := systemdFields(output)
	if platform == "darwin" {
		values = launchctlFields(output)
	}
	state, pid := values["ActiveState"], values["MainPID"]
	if platform == "darwin" {
		state, pid = values["state"], values["pid"]
	}
	process, err := strconv.Atoi(pid)
	if err != nil || process <= 0 {
		return errors.New(name + " has no running process")
	}
	if platform == "darwin" {
		if state != "running" {
			return errors.New(name + " LaunchAgent is loaded but not running")
		}
	} else if state != "active" || values["SubState"] != "running" {
		return errors.New(name + " is not running")
	}
	return nil
}

func serviceKind(args []string) string {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "ohya-serve.service") || strings.Contains(joined, "com.nibzard.ohya.serve") {
		return "hub service"
	}
	return "memory service"
}

func RunServiceLoaded(ctx context.Context, args []string) error {
	output, err := serviceOutput(ctx, args)
	if err != nil {
		return err
	}
	if args[0] == "systemctl" && strings.TrimSpace(output) != "loaded" {
		return errors.New(serviceKind(args) + " is not loaded")
	}
	return nil
}

func RunServiceCommand(ctx context.Context, args []string) error {
	output, err := serviceOutput(ctx, args)
	if err != nil {
		return err
	}
	if args[0] == "launchctl" && len(args) > 1 && args[1] == "print" {
		return serviceRunning("darwin", output, serviceKind(args))
	}
	if args[0] == "systemctl" && strings.Contains(strings.Join(args, " "), "--property=ActiveState,SubState,MainPID") {
		return serviceRunning("linux", output, serviceKind(args))
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
