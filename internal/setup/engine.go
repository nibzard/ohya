package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/store"
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
	Hub     *HubJoin `json:"hub,omitempty"`
}

type HubJoin struct {
	Code          string `json:"code_secret"`
	ExpiresAt     string `json:"expires_at"`
	DeviceCommand string `json:"device_command"`
	RelayRegion   string `json:"relay_region"`
	NodeKey       string `json:"node_key"`
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

	HubServiceArgs []string
	CheckHub       func(context.Context) error
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
		service, err = ServicePlan(platform, o.Clients.Home, o.Clients.State, o.Clients.Binary, o.ServiceArgs)
		if err != nil {
			return report, err
		}
	}
	var hubService Service
	if !o.NoService && o.HubServiceArgs != nil {
		hubService, err = HubServicePlan(platform, o.Clients.Home, o.Clients.State, o.Clients.Binary, o.HubServiceArgs)
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
		if hubService.Path != "" {
			report.add(o, Result{Component: "hub-service", Status: "planned", Detail: hubService.Path})
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
		if report.Complete() && hubService.Path != "" {
			report.add(o, configureOwnedService(ctx, o, hubService, "hub-service", "hub-service.json", false)...)
			if !report.Complete() {
				return report, nil
			}
			if o.CheckHub != nil {
				check := Result{Component: "hub", Status: "ready", Detail: "The hub reaches its relay and answers a device dial."}
				if err := o.CheckHub(ctx); err != nil {
					check.Status = "failed"
					check.Detail = err.Error()
					report.add(o, check)
					return report, nil
				}
				report.add(o, check)
			}
		}
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
			if err == nil {
				var skillChanged bool
				skillChanged, err = ApplySkill(o.Clients, c)
				changed = changed || skillChanged
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
		result, err := removeOwnedService(ctx, r, run, "service", serviceReceipt, ServicePlan)
		if err != nil {
			return report, err
		}
		report.Results = append(report.Results, result)
	} else if !isMissing(err) {
		return report, fmt.Errorf("read service receipt: %w", err)
	}
	hubServiceReceipt := filepath.Join(state, "setup", "hub-service.json")
	if r, err := loadFileReceipt(hubServiceReceipt); err == nil {
		result, err := removeOwnedService(ctx, r, run, "hub-service", hubServiceReceipt, HubServicePlan)
		if err != nil {
			return report, err
		}
		report.Results = append(report.Results, result)
	} else if !isMissing(err) {
		return report, fmt.Errorf("read hub service receipt: %w", err)
	}
	profiles, err := RemoveProfiles(state)
	if err != nil {
		return report, err
	}
	report.Results = append(report.Results, profiles...)
	for _, name := range []string{"claude", "codex", "pi", "agy"} {
		clientChanged, clientErr := RemoveClient(state, name)
		skillChanged, skillErr := RemoveSkill(state, name)
		result := Result{Component: name, Status: "removed", Changed: clientChanged || skillChanged}
		if clientErr != nil || skillErr != nil {
			result.Status = "failed"
			switch {
			case clientErr != nil && skillErr != nil:
				result.Detail = clientErr.Error() + "; " + skillErr.Error()
			case clientErr != nil:
				result.Detail = clientErr.Error()
			default:
				result.Detail = skillErr.Error()
			}
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

func removeOwnedService(ctx context.Context, r fileReceipt, run func(context.Context, []string) error, component, receiptPath string, plan func(string, string, string, string, []string) (Service, error)) (Result, error) {
	platform := "linux"
	if filepath.Ext(r.Path) == ".plist" {
		platform = "darwin"
	}
	service, err := plan(platform, "/", "/", "/ohya", nil)
	if err != nil {
		return Result{}, err
	}
	result := Result{Component: component, Status: "removed"}
	current, readErr := readRegular(r.Path)
	if readErr != nil && !isMissing(readErr) {
		return result, readErr
	}
	if readErr == nil && !bytes.Equal(current, r.Content) && !bytes.Equal(current, r.Previous) {
		return result, fmt.Errorf("%s has been edited; preserve its service", r.Path)
	}
	for _, cmd := range service.Stop {
		if err = run(ctx, cmd); err != nil {
			break
		}
	}
	if err == nil {
		result.Changed, err = RemoveFile(receiptPath)
	}
	if err != nil {
		result.Status = "failed"
		result.Detail = err.Error()
	}
	return result, nil
}

const recoveryPointsKept = 2
const recoverySpaceMarginBytes = 64 << 20

const recoveryStagingPrefix = ".recovery-"

var ErrInsufficientSpace = errors.New("insufficient free space for the recovery point")

type CopySpace struct {
	Size     uint64
	Required uint64
	Free     uint64
}

func MeasureCopySpace(database, targetDir string) (CopySpace, error) {
	info, err := os.Lstat(database)
	if err != nil {
		return CopySpace{}, err
	}
	size := uint64(info.Size())
	if wal, err := os.Lstat(database + "-wal"); err == nil && wal.Mode().IsRegular() {
		size += uint64(wal.Size())
	}
	free, err := availableBytes(targetDir)
	if err != nil {
		return CopySpace{}, err
	}
	return CopySpace{Size: size, Required: size + size/10 + recoverySpaceMarginBytes, Free: free}, nil
}

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
	var cleanupErr error
	if _, err := os.Lstat(dir); err == nil {
		cleanupErr = removeAbandonedStaging(dir)
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	space, err := MeasureCopySpace(database, dir)
	if err != nil {
		return Result{}, err
	}
	if space.Free < space.Required {
		detail := fmt.Sprintf("only %s free on %s for a recovery point of about %s",
			FormatBytes(int64(space.Free)), dir, FormatBytes(int64(space.Required)))
		result := Result{Component: "backup", Status: "failed", Detail: detail}
		return result, fmt.Errorf("%w: %s; remove old recovery points under %s or free space, then rerun ohya setup (setup --state can use a disk with more space)",
			ErrInsufficientSpace, detail, dir)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Result{}, err
	}
	staging, err := os.MkdirTemp(dir, recoveryStagingPrefix)
	if err != nil {
		return Result{}, err
	}
	point := filepath.Join(dir, "recovery-"+strings.TrimPrefix(filepath.Base(staging), recoveryStagingPrefix))
	path := filepath.Join(staging, "hub.db")
	result := Result{Component: "backup", Status: "saved", Detail: filepath.Join(point, "hub.db"), Changed: true}
	o.progress(fmt.Sprintf("saving recovery point (about %s) to %s…", FormatBytes(int64(space.Size)), dir))
	if err := store.Backup(ctx, database, path, recoveryProgress(o)); err != nil {
		_ = os.RemoveAll(staging)
		result.Status = "failed"
		result.Detail = "could not create a consistent recovery point"
		result.Changed = false
		return result, err
	}
	if err := os.Rename(staging, point); err != nil {
		_ = os.RemoveAll(staging)
		result.Status = "failed"
		result.Detail = "could not publish the recovery point"
		result.Changed = false
		return result, err
	}
	if cleanupErr != nil {
		result.Detail += "; abandoned staging directories could not be removed: " + cleanupErr.Error()
	}
	if err := pruneRecoveryPoints(dir, recoveryPointsKept); err != nil {
		result.Detail += "; older recovery points could not be removed: " + err.Error()
	}
	return result, nil
}

func migrateDatabase(ctx context.Context, o Options) (Result, error) {
	from, err := appliedSchemaVersion(ctx, o.Database)
	if err != nil {
		return Result{Component: "schema", Status: "failed", Detail: err.Error()}, err
	}
	if from < 0 {
		return Result{}, nil
	}
	want, err := store.SchemaVersion()
	if err != nil {
		return Result{}, err
	}
	if from == want {
		return Result{}, nil
	}
	o.progress(fmt.Sprintf("migrating the database from schema %d to %d…", from, want))
	hub, err := store.Open(ctx, o.Database)
	if err != nil {
		return Result{Component: "schema", Status: "failed", Detail: "database migration failed: " + err.Error()}, err
	}
	defer hub.Close()
	return Result{Component: "schema", Status: "updated", Changed: true,
		Detail: fmt.Sprintf("database schema %d is now %d", from, want)}, nil
}

func appliedSchemaVersion(ctx context.Context, database string) (int, error) {
	if database == "" {
		return -1, nil
	}
	if _, err := os.Lstat(database); err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return 0, err
	}
	ro, err := store.OpenReadOnly(database)
	if err != nil {
		return 0, err
	}
	defer ro.Close()
	version, _, err := ro.AppliedSchemaVersion(ctx)
	if errors.Is(err, store.ErrNoSchemaMigrations) {
		return -1, nil
	}
	return version, err
}

func recoveryProgress(o Options) func(done, total int64) {
	lastDecile := 0
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		percent := int(done * 100 / total)
		decile := percent / 10
		if decile <= lastDecile || decile >= 10 {
			return
		}
		lastDecile = decile
		o.progress(fmt.Sprintf("recovery point %d%% (%s of %s)", percent, FormatBytes(done), FormatBytes(total)))
	}
}

func removeAbandonedStaging(dir string) error {
	stale, err := filepath.Glob(filepath.Join(dir, recoveryStagingPrefix+"*"))
	if err != nil {
		return err
	}
	for _, path := range stale {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func availableBytes(path string) (uint64, error) {
	for {
		var stat syscall.Statfs_t
		err := syscall.Statfs(path, &stat)
		if err == nil {
			return stat.Bavail * uint64(stat.Bsize), nil
		}
		if !os.IsNotExist(err) {
			return 0, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, err
		}
		path = parent
	}
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

func FormatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
}
