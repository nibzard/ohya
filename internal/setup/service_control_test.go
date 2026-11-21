package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedOwnedServiceFixture(t *testing.T, platform string, hub bool) (home, state, unitPath string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	state = filepath.Join(root, "state")
	planFn := ServicePlan
	receiptName := "service.json"
	if hub {
		planFn = HubServicePlan
		receiptName = "hub-service.json"
	}
	service, err := planFn(platform, home, state, "/usr/local/bin/ohya", []string{"ingest"})
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(state, "setup", receiptName)
	if _, err := ApplyFile(service.Path, receiptPath, service.Content); err != nil {
		t.Fatal(err)
	}
	return home, state, service.Path
}

func TestControlServiceStatusWhenNotConfigured(t *testing.T) {
	state := t.TempDir()
	got, err := ControlService(context.Background(), ServiceActionStatus, ServiceControlOptions{
		State: state,
		Home:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("status error: %v", err)
	}
	if got.Service != "service" || got.Installed || got.Loaded || got.Running || got.Changed {
		t.Fatalf("unexpected status: %+v", got)
	}
	if !strings.Contains(got.Detail, "ohya setup") {
		t.Fatalf("detail = %q; want setup hint", got.Detail)
	}
	if _, err := ControlService(context.Background(), ServiceActionStart, ServiceControlOptions{
		State: state,
		Home:  t.TempDir(),
	}); !errors.Is(err, ErrServiceNotConfigured) {
		t.Fatalf("start err = %v; want ErrServiceNotConfigured", err)
	}
}

func TestControlServiceDarwinLifecycleAndEditedPreservation(t *testing.T) {
	home, state, unitPath := seedOwnedServiceFixture(t, "darwin", false)
	loaded := false
	running := false
	var ran []string

	opts := ServiceControlOptions{
		State: state,
		Home:  home,
		LoadedService: func(_ context.Context, _ []string) error {
			if loaded {
				return nil
			}
			return errors.New("not loaded")
		},
		RunService: func(_ context.Context, args []string) error {
			cmd := strings.Join(args, " ")
			ran = append(ran, cmd)
			if len(args) >= 2 && args[0] == "launchctl" {
				switch args[1] {
				case "bootstrap":
					loaded = true
				case "kickstart":
					running = true
				case "bootout":
					loaded = false
					running = false
				case "print":
					if !running {
						return errors.New("not running")
					}
				}
			}
			return nil
		},
		Wait: func(_ context.Context, _ Service, _ func(context.Context, []string) error) error {
			if !running {
				return errors.New("wait saw not running")
			}
			return nil
		},
	}

	got, err := ControlService(context.Background(), ServiceActionStart, opts)
	if err != nil {
		t.Fatalf("start error: %v", err)
	}
	if !got.Installed || !got.Loaded || !got.Running || !got.Changed || got.Path != unitPath {
		t.Fatalf("start status = %+v", got)
	}
	if len(ran) == 0 || !strings.Contains(ran[0], "launchctl bootstrap") || !strings.Contains(ran[0], unitPath) {
		t.Fatalf("first command = %v; want launchctl bootstrap targeting %s", ran, unitPath)
	}

	ran = nil
	got, err = ControlService(context.Background(), ServiceActionStart, opts)
	if err != nil {
		t.Fatalf("idempotent start error: %v", err)
	}
	if got.Changed || !got.Running || len(ran) != 1 {
		t.Fatalf("idempotent start = %+v, ran=%v", got, ran)
	}

	if err := os.WriteFile(unitPath, []byte("<plist>edited</plist>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran = nil
	got, err = ControlService(context.Background(), ServiceActionRestart, opts)
	if err != nil {
		t.Fatalf("restart on edited unit error: %v", err)
	}
	if !got.Edited || !got.Changed || !got.Running {
		t.Fatalf("restart status = %+v", got)
	}
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "<plist>edited</plist>\n" {
		t.Fatalf("restart overwrote edited unit file: %q", string(raw))
	}
	if len(ran) < 3 || !strings.Contains(ran[1], "bootout") || !strings.Contains(ran[2], "bootstrap") {
		t.Fatalf("restart commands = %v; want check, bootout, bootstrap, kickstart", ran)
	}

	ran = nil
	got, err = ControlService(context.Background(), ServiceActionStop, opts)
	if err != nil {
		t.Fatalf("stop error: %v", err)
	}
	if !got.Changed || got.Loaded || got.Running {
		t.Fatalf("stop status = %+v", got)
	}

	ran = nil
	got, err = ControlService(context.Background(), ServiceActionStop, opts)
	if err != nil {
		t.Fatalf("idempotent stop error: %v", err)
	}
	if got.Changed || len(ran) != 0 {
		t.Fatalf("idempotent stop = %+v, ran=%v", got, ran)
	}
}

func TestControlServiceHubMissingUnitRefusesStartAllowsStop(t *testing.T) {
	home, state, unitPath := seedOwnedServiceFixture(t, "linux", true)
	if err := os.Remove(unitPath); err != nil {
		t.Fatal(err)
	}
	loaded := true
	opts := ServiceControlOptions{
		State: state,
		Home:  home,
		Hub:   true,
		LoadedService: func(_ context.Context, _ []string) error {
			if loaded {
				return nil
			}
			return errors.New("not loaded")
		},
		RunService: func(_ context.Context, args []string) error {
			if strings.Contains(strings.Join(args, " "), "disable --now") {
				loaded = false
			}
			return nil
		},
		Wait: func(_ context.Context, _ Service, _ func(context.Context, []string) error) error { return nil },
	}

	status, err := ControlService(context.Background(), ServiceActionStatus, opts)
	if err != nil {
		t.Fatalf("status error: %v", err)
	}
	if status.Service != "hub-service" || status.Installed || !strings.Contains(status.Detail, "missing") {
		t.Fatalf("status = %+v", status)
	}

	if _, err := ControlService(context.Background(), ServiceActionStart, opts); !errors.Is(err, ErrServiceUnitMissing) {
		t.Fatalf("start err = %v; want ErrServiceUnitMissing", err)
	}

	stopped, err := ControlService(context.Background(), ServiceActionStop, opts)
	if err != nil {
		t.Fatalf("stop missing-unit error: %v", err)
	}
	if !stopped.Changed || stopped.Loaded || stopped.Installed {
		t.Fatalf("stop = %+v", stopped)
	}
}
