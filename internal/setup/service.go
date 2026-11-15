package setup

import (
	"context"
	"encoding/xml"
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
		return Service{Path: filepath.Join(home, ".config", "systemd", "user", "ohya.service"), Content: []byte(content), Start: [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "ohya.service"}, {"systemctl", "--user", "restart", "ohya.service"}}, Stop: [][]string{{"systemctl", "--user", "disable", "--now", "ohya.service"}}, Check: []string{"systemctl", "--user", "is-active", "--quiet", "ohya.service"}}, nil
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
		return Service{Path: path, Content: []byte(b.String()), Start: [][]string{{"launchctl", "bootstrap", domain, path}, {"launchctl", "kickstart", "-k", domain + "/com.nibzard.ohya"}}, Stop: [][]string{{"launchctl", "bootout", domain + "/com.nibzard.ohya"}}, Check: []string{"launchctl", "print", domain + "/com.nibzard.ohya"}}, nil
	default:
		return Service{}, fmt.Errorf("user services are not supported on %s", platform)
	}
}

func RunServiceCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("empty service command")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = os.TempDir()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w; check your user service manager", strings.Join(args, " "), err)
	}
	return nil
}
