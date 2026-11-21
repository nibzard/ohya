package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	ErrServiceNotConfigured = errors.New("owned service is not configured")
	ErrServiceUnitMissing   = errors.New("owned service unit file is missing")
)

type ServiceAction string

const (
	ServiceActionStatus  ServiceAction = "status"
	ServiceActionStart   ServiceAction = "start"
	ServiceActionStop    ServiceAction = "stop"
	ServiceActionRestart ServiceAction = "restart"
)

type ServiceControlOptions struct {
	State         string
	Home          string
	Hub           bool
	LoadedService func(context.Context, []string) error
	RunService    func(context.Context, []string) error
	Wait          func(context.Context, Service, func(context.Context, []string) error) error
}

type ServiceStatus struct {
	Service   string `json:"service"`
	Action    string `json:"action"`
	Installed bool   `json:"installed"`
	Edited    bool   `json:"edited"`
	Loaded    bool   `json:"loaded"`
	Running   bool   `json:"running"`
	Changed   bool   `json:"changed"`
	Path      string `json:"path,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

func ControlService(ctx context.Context, action ServiceAction, o ServiceControlOptions) (ServiceStatus, error) {
	switch action {
	case ServiceActionStatus, ServiceActionStart, ServiceActionStop, ServiceActionRestart:
	default:
		return ServiceStatus{}, fmt.Errorf("unknown service action %q", action)
	}
	serviceName := "service"
	receiptName := "service.json"
	planFn := ServicePlan
	setupHint := "run ohya setup"
	if o.Hub {
		serviceName = "hub-service"
		receiptName = "hub-service.json"
		planFn = HubServicePlan
		setupHint = "run ohya setup --hub"
	}
	loadedFn := o.LoadedService
	if loadedFn == nil {
		loadedFn = RunServiceLoaded
	}
	runFn := o.RunService
	if runFn == nil {
		runFn = RunServiceCommand
	}
	waitFn := o.Wait
	if waitFn == nil {
		waitFn = waitService
	}
	if action != ServiceActionStatus {
		unlock, err := Lock(o.State)
		if err != nil {
			return ServiceStatus{}, err
		}
		defer unlock()
	}
	receiptPath := filepath.Join(o.State, "setup", receiptName)
	receipt, err := loadFileReceipt(receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		if action == ServiceActionStatus {
			return ServiceStatus{
				Service:   serviceName,
				Action:    string(action),
				Installed: false,
				Detail:    "not configured; " + setupHint,
			}, nil
		}
		return ServiceStatus{}, fmt.Errorf("%w: %s", ErrServiceNotConfigured, serviceName)
	}
	if err != nil {
		return ServiceStatus{}, err
	}
	current, readErr := readRegular(receipt.Path)
	unitMissing := errors.Is(readErr, os.ErrNotExist)
	if readErr != nil && !unitMissing {
		return ServiceStatus{}, readErr
	}
	edited := !unitMissing && !bytes.Equal(current, receipt.Content) && !bytes.Equal(current, receipt.Previous)

	platform := "linux"
	if filepath.Ext(receipt.Path) == ".plist" {
		platform = "darwin"
	}
	home := o.Home
	if home == "" {
		home = "/"
	}
	service, err := planFn(platform, home, o.State, "/ohya", nil)
	if err != nil {
		return ServiceStatus{}, err
	}
	service.Path = receipt.Path
	if platform == "darwin" && len(service.Start) > 0 && len(service.Start[0]) == 4 {
		service.Start[0][3] = receipt.Path
	}

	loaded := loadedFn(ctx, service.Loaded) == nil
	running := false
	if loaded {
		running = runFn(ctx, service.Check) == nil
	}

	switch action {
	case ServiceActionStatus:
		res := ServiceStatus{
			Service:   serviceName,
			Action:    string(action),
			Installed: !unitMissing,
			Edited:    edited,
			Loaded:    loaded,
			Running:   running,
			Path:      receipt.Path,
		}
		switch {
		case unitMissing:
			res.Detail = "service unit file is missing; " + setupHint + " to recreate it"
		case !loaded:
			res.Detail = "the service manager reports the unit as not loaded"
		case !running:
			res.Detail = "the service is loaded but its check command reports no running process"
		case edited:
			res.Detail = "unit file was edited after setup; preserved"
		}
		return res, nil

	case ServiceActionStart:
		if unitMissing {
			return ServiceStatus{}, fmt.Errorf("%w: %s", ErrServiceUnitMissing, receipt.Path)
		}
		if loaded && running {
			return ServiceStatus{
				Service:   serviceName,
				Action:    string(action),
				Installed: true,
				Edited:    edited,
				Loaded:    true,
				Running:   true,
				Changed:   false,
				Path:      receipt.Path,
				Detail:    "already running",
			}, nil
		}
		if loaded {
			if err := runServiceCommands(ctx, service.Stop, runFn); err != nil {
				return ServiceStatus{}, err
			}
		}
		if err := PrepareStderrLog(service.StderrPath); err != nil {
			return ServiceStatus{}, err
		}
		if err := runServiceCommands(ctx, service.Start, runFn); err != nil {
			return ServiceStatus{}, err
		}
		if err := waitFn(ctx, service, runFn); err != nil {
			return ServiceStatus{}, err
		}
		return ServiceStatus{
			Service:   serviceName,
			Action:    string(action),
			Installed: true,
			Edited:    edited,
			Loaded:    true,
			Running:   true,
			Changed:   true,
			Path:      receipt.Path,
		}, nil

	case ServiceActionStop:
		if !loaded && !running {
			return ServiceStatus{
				Service:   serviceName,
				Action:    string(action),
				Installed: !unitMissing,
				Edited:    edited,
				Loaded:    false,
				Running:   false,
				Changed:   false,
				Path:      receipt.Path,
				Detail:    "already stopped",
			}, nil
		}
		if err := runServiceCommands(ctx, service.Stop, runFn); err != nil {
			return ServiceStatus{}, err
		}
		return ServiceStatus{
			Service:   serviceName,
			Action:    string(action),
			Installed: !unitMissing,
			Edited:    edited,
			Loaded:    false,
			Running:   false,
			Changed:   true,
			Path:      receipt.Path,
		}, nil

	case ServiceActionRestart:
		if unitMissing {
			return ServiceStatus{}, fmt.Errorf("%w: %s", ErrServiceUnitMissing, receipt.Path)
		}
		if loaded {
			if err := runServiceCommands(ctx, service.Stop, runFn); err != nil {
				return ServiceStatus{}, err
			}
		}
		if err := PrepareStderrLog(service.StderrPath); err != nil {
			return ServiceStatus{}, err
		}
		if err := runServiceCommands(ctx, service.Start, runFn); err != nil {
			return ServiceStatus{}, err
		}
		if err := waitFn(ctx, service, runFn); err != nil {
			return ServiceStatus{}, err
		}
		return ServiceStatus{
			Service:   serviceName,
			Action:    string(action),
			Installed: true,
			Edited:    edited,
			Loaded:    true,
			Running:   true,
			Changed:   true,
			Path:      receipt.Path,
		}, nil
	}
	return ServiceStatus{}, fmt.Errorf("unknown service action %q", action)
}

func runServiceCommands(ctx context.Context, commands [][]string, run func(context.Context, []string) error) error {
	for _, command := range commands {
		if err := run(ctx, command); err != nil {
			return err
		}
	}
	return nil
}
