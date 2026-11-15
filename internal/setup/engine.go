package setup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

type Result struct {
	Component string `json:"component"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Changed   bool   `json:"changed"`
}

type Report struct {
	Results []Result `json:"results"`
	DryRun  bool     `json:"dry_run"`
}

func (r Report) Complete() bool {
	for _, item := range r.Results {
		switch item.Status {
		case "failed", "unsupported", "blocked":
			return false
		}
	}
	return true
}

type Options struct {
	Clients     ClientOptions
	ServiceArgs []string
	Platform    string
	NoService   bool
	NoAgents    bool
	DryRun      bool
	RunService  func(context.Context, []string) error
	Verify      func(context.Context, Command) error
	ProbePi     func(context.Context, string) error
}

func Run(ctx context.Context, o Options) (Report, error) {
	report := Report{DryRun: o.DryRun, Results: []Result{}}
	clients := []Client{}
	var err error
	if !o.NoAgents {
		clients, err = DiscoverClients(o.Clients)
		if err != nil {
			return report, err
		}
	}
	platform := o.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	var service Service
	if !o.NoService {
		service, err = ServicePlan(platform, o.Clients.Home, o.Clients.Binary, o.ServiceArgs)
		if err != nil {
			return report, err
		}
	}
	if o.DryRun {
		if !o.NoService {
			report.Results = append(report.Results, Result{Component: "service", Status: "planned", Detail: service.Path})
		}
		for _, c := range clients {
			report.Results = append(report.Results, Result{Component: c.Name, Status: c.Status, Detail: c.Config})
		}
		return report, nil
	}
	unlock, err := Lock(o.Clients.State)
	if err != nil {
		return report, err
	}
	defer unlock()
	if o.RunService == nil {
		o.RunService = RunServiceCommand
	}
	if o.Verify == nil {
		o.Verify = VerifyCommand
	}
	if o.ProbePi == nil {
		o.ProbePi = ProbeNativePi
	}
	if !o.NoService {
		result := Result{Component: "service", Status: "failed"}
		receipt := filepath.Join(o.Clients.State, "setup", "service.json")
		var changed bool
		_, receiptErr := readRegular(receipt)
		active := o.RunService(ctx, service.Check) == nil
		var err error
		if active && os.IsNotExist(receiptErr) {
			err = fmt.Errorf("an unowned ohya service is already loaded")
		} else {
			changed, err = ApplyFile(service.Path, receipt, service.Content)
		}
		result.Changed = changed
		if err == nil {
			if changed && active {
				for _, cmd := range service.Stop {
					if err = o.RunService(ctx, cmd); err != nil {
						break
					}
				}
				active = false
			}
			if err == nil && !active {
				for _, cmd := range service.Start {
					if err = o.RunService(ctx, cmd); err != nil {
						break
					}
				}
			}
			if err == nil {
				err = o.RunService(ctx, service.Check)
			}
		}
		if err != nil {
			result.Detail = err.Error()
		} else {
			result.Status = "running"
			result.Detail = "History imports in the background; run ohya doctor to check sources."
		}
		report.Results = append(report.Results, result)
	}
	command := Command{Command: o.Clients.Binary, Args: o.Clients.Args}
	verified := o.Verify(ctx, command)
	check := Result{Component: "memory", Status: "ready"}
	if verified != nil {
		check.Status = "failed"
		check.Detail = verified.Error()
	}
	report.Results = append(report.Results, check)
	for _, c := range clients {
		result := Result{Component: c.Name, Status: c.Status, Detail: c.Detail}
		if c.Status == "planned" {
			if c.Name == "pi" {
				if err := o.ProbePi(ctx, c.Executable); err != nil {
					result.Status = "unsupported"
					result.Detail = err.Error()
					report.Results = append(report.Results, result)
					continue
				}
			}
			changed, err := ApplyClient(o.Clients, c)
			result.Changed = changed
			if err != nil {
				result.Status = "failed"
				result.Detail = err.Error()
			} else if verified != nil {
				result.Status = "failed"
				result.Detail = "Configured, but the memory connection failed; rerun ohya setup."
			} else {
				result.Status = "configured"
				result.Detail = "Open a new agent session to load ohya."
			}
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func Remove(ctx context.Context, state string, run func(context.Context, []string) error) (Report, error) {
	report := Report{Results: []Result{}}
	unlock, err := Lock(state)
	if err != nil {
		return report, err
	}
	defer unlock()
	if run == nil {
		run = RunServiceCommand
	}
	serviceReceipt := filepath.Join(state, "setup", "service.json")
	if r, err := loadFileReceipt(serviceReceipt); err == nil {
		platform := "linux"
		if filepath.Ext(r.Path) == ".plist" {
			platform = "darwin"
		}
		service, err := ServicePlan(platform, "/", "/ohya", nil)
		if err != nil {
			return report, err
		}
		result := Result{Component: "service", Status: "removed"}
		current, readErr := readRegular(r.Path)
		if readErr != nil && !isMissing(readErr) {
			return report, readErr
		}
		if readErr == nil && !bytes.Equal(current, r.Content) && !bytes.Equal(current, r.Previous) {
			return report, fmt.Errorf("%s has been edited; preserve its service", r.Path)
		}
		for _, cmd := range service.Stop {
			if err = run(ctx, cmd); err != nil {
				break
			}
		}
		if err == nil {
			result.Changed, err = RemoveFile(serviceReceipt)
		}
		if err != nil {
			result.Status = "failed"
			result.Detail = err.Error()
		}
		report.Results = append(report.Results, result)
	} else if !isMissing(err) {
		return report, fmt.Errorf("read service receipt: %w", err)
	}
	for _, name := range []string{"claude", "codex", "pi"} {
		changed, err := RemoveClient(state, name)
		result := Result{Component: name, Status: "removed", Changed: changed}
		if err != nil {
			result.Status = "failed"
			result.Detail = err.Error()
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}
