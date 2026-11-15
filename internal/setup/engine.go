package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/nibzard/ohya/internal/store"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"
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
	WorkingDirectory string
	NoModifyPath     bool
	Shell            string
	Database         string
	Clients          ClientOptions
	ServiceArgs      []string
	Platform         string
	NoService        bool
	NoAgents         bool
	DryRun           bool
	RunService       func(context.Context, []string) error
	LoadedService    func(context.Context, []string) error
	Verify           func(context.Context, Command) error
	ProbePi          func(context.Context, string) error
	Prepare          func(context.Context) error
	Notify           func(Result)
	Progress         func(string)
}

func (o Options) progress(line string) {
	if o.Progress != nil {
		o.Progress(line)
	}
}

func (r *Report) add(o Options, results ...Result) {
	for _, result := range results {
		r.Results = append(r.Results, result)
		if o.Notify != nil {
			o.Notify(result)
		}
	}
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
	for i, client := range clients {
		if client.Status != "planned" {
			continue
		}
		if err := PreflightClient(o.Clients, client, o.WorkingDirectory); err != nil {
			clients[i].Status = "blocked"
			clients[i].Detail = err.Error()
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
	var profiles []Profile
	if !o.NoModifyPath {
		profiles, err = ProfilePlan(o.Clients.Home, o.Shell, filepath.Dir(o.Clients.Binary))
		if err != nil {
			return report, err
		}
	}
	if o.DryRun {
		for _, p := range profiles {
			report.add(o, Result{Component: "path", Status: "planned", Detail: p.Path})
		}
		if !o.NoService {
			report.add(o, Result{Component: "service", Status: "planned", Detail: service.Path})
		}
		for _, c := range clients {
			detail := c.Config
			if c.Status == "blocked" {
				detail = c.Detail
			}
			report.add(o, Result{Component: c.Name, Status: c.Status, Detail: detail})
		}
		return report, nil
	}
	blocked := false
	for _, client := range clients {
		if client.Status == "blocked" {
			blocked = true
		}
	}
	if blocked {
		for _, client := range clients {
			report.add(o, Result{Component: client.Name, Status: client.Status, Detail: client.Detail})
		}
		return report, nil
	}
	unlock, err := Lock(o.Clients.State)
	if err != nil {
		return report, err
	}
	defer unlock()
	if o.Prepare != nil {
		if err := o.Prepare(ctx); err != nil {
			return report, err
		}
	}
	if o.LoadedService == nil {
		if o.RunService != nil {
			o.LoadedService = o.RunService
		} else {
			o.LoadedService = RunServiceLoaded
		}
	}
	if o.RunService == nil {
		o.RunService = RunServiceCommand
	}
	if o.Verify == nil {
		o.Verify = VerifyCommand
	}
	if o.ProbePi == nil {
		o.ProbePi = ProbeNativePi
	}
	for _, p := range profiles {
		changed, err := ApplyProfile(o.Clients.State, p)
		result := Result{Component: "path", Status: "configured", Detail: p.Path, Changed: changed}
		if err != nil {
			result.Status = "failed"
			result.Detail = err.Error()
		}
		report.add(o, result)
	}
	if !o.NoService {
		report.add(o, configureService(ctx, o, service)...)
	}
	if o.NoService {
		recovery, err := recoveryPoint(ctx, o)
		if recovery.Component != "" {
			report.add(o, recovery)
		}
		if err != nil {
			return report, err
		}
	}
	if !report.Complete() {
		return report, nil
	}
	command := Command{Command: o.Clients.Binary, Args: o.Clients.Args}
	verified := o.Verify(ctx, command)
	check := Result{Component: "memory", Status: "ready"}
	if verified != nil {
		check.Status = "failed"
		check.Detail = verified.Error()
	}
	report.add(o, check)
	for _, c := range clients {
		result := Result{Component: c.Name, Status: c.Status, Detail: c.Detail}
		if c.Status == "planned" {
			legacyPi := false
			if c.Name == "pi" {
				if err := o.ProbePi(ctx, c.Executable); err != nil {
					if errors.Is(err, ErrPiNeedsExtension) {
						legacyPi = true
					} else {
						result.Status = "failed"
						result.Detail = err.Error()
						report.add(o, result)
						continue
					}
				}
			}
			var changed bool
			var err error
			if legacyPi {
				_, err = RemoveClient(o.Clients.State, "pi")
				if err == nil {
					changed, err = ApplyPiExtension(o.Clients, c)
				}
			} else {
				if c.Name == "pi" {
					_, err = RemovePiExtension(o.Clients.State)
				}
				if err == nil {
					changed, err = ApplyClient(o.Clients, c)
				}
			}
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
		report.add(o, result)
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
	profiles, err := RemoveProfiles(state)
	if err != nil {
		return report, err
	}
	report.Results = append(report.Results, profiles...)
	for _, name := range []string{"claude", "codex", "pi"} {
		changed, err := RemoveClient(state, name)
		result := Result{Component: name, Status: "removed", Changed: changed}
		if err != nil {
			result.Status = "failed"
			result.Detail = err.Error()
		}
		report.Results = append(report.Results, result)
	}
	changed, err := RemovePiExtension(state)
	if err != nil {
		report.Results = append(report.Results, Result{Component: "pi-extension", Status: "failed", Detail: err.Error()})
	} else if changed {
		report.Results = append(report.Results, Result{Component: "pi-extension", Status: "removed", Changed: true})
	}
	return report, nil
}

const recoveryPointsKept = 2

func recoveryPoint(ctx context.Context, o Options) (Result, error) {
	database := o.Database
	if database == "" {
		return Result{}, nil
	}
	info, err := os.Lstat(database)
	if os.IsNotExist(err) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("memory database is not a regular file")
	}
	if current, err := store.SchemaCurrent(ctx, database); err == nil && current {
		return Result{Component: "backup", Status: "not needed", Detail: "memory schema matches this build"}, nil
	}
	dir := filepath.Join(o.Clients.State, "setup", "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Result{}, err
	}
	point, err := os.MkdirTemp(dir, "recovery-")
	if err != nil {
		return Result{}, err
	}
	path := filepath.Join(point, "hub.db")
	result := Result{Component: "backup", Status: "saved", Detail: path, Changed: true}
	o.progress(fmt.Sprintf("saving recovery point (%s)…", formatBytes(info.Size())))
	if err := store.Backup(ctx, database, path); err != nil {
		result.Status = "failed"
		result.Detail = "could not create a consistent recovery point"
		result.Changed = false
		return result, err
	}
	if err := pruneRecoveryPoints(dir, recoveryPointsKept); err != nil {
		result.Detail += "; older recovery points could not be removed: " + err.Error()
	}
	return result, nil
}

func pruneRecoveryPoints(dir string, keep int) error {
	points, err := filepath.Glob(filepath.Join(dir, "recovery-*"))
	if err != nil {
		return err
	}
	type dated struct {
		path     string
		modified time.Time
	}
	var found []dated
	for _, point := range points {
		info, err := os.Lstat(point)
		if err != nil {
			return err
		}
		if info.IsDir() {
			found = append(found, dated{point, info.ModTime()})
		}
	}
	slices.SortFunc(found, func(a, b dated) int { return b.modified.Compare(a.modified) })
	for _, old := range found[min(keep, len(found)):] {
		if err := os.RemoveAll(old.path); err != nil {
			return err
		}
	}
	return nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
}
