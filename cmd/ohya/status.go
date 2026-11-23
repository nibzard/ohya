package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/antigravity"
	"github.com/nibzard/ohya/internal/buildinfo"
	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/setup"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/transport/address"
)

const statusFormatVersion = 1

var statusSpec = commandSpec{
	name:      "ohya status",
	synopsis:  "report runtime state from published local snapshots",
	usageLine: "ohya status [flags]",
	description: "Reads the cached snapshots every long-running ohya process publishes under the state directory's observe/ tree, together with the saved setup connection and owned service receipts. " +
		"It never contacts the hub, starts or restarts a service, migrates a database, or repairs files. " +
		"A snapshot older than thirty seconds is stale: a valid recent sample is evidence of recent activity, not proof of liveness, and process liveness is probed separately. " +
		"Multiple matching instances are listed separately and their counters are never merged. " +
		"With --remote the command also opens one short-lived authenticated connection to fetch cached hub aggregate status (version, uptime, storage sizes, resources, limits) without reading logs or transcripts. " +
		"Exit codes: 0 for a complete fresh result, 3 for a valid but partial, stale, or degraded result, 1 for failure, 2 for usage.",
	examples: []string{"ohya status --json", "ohya status --state ~/.ohya/join --role join"},
}

type statusIdentity struct {
	Version  string         `json:"version"`
	Revision string         `json:"revision"`
	Dirty    string         `json:"dirty"`
	Go       string         `json:"go"`
	OS       string         `json:"os"`
	Arch     string         `json:"arch"`
	Schema   int            `json:"schema"`
	Parsers  map[string]int `json:"parsers"`
}

type statusAgentRoot struct {
	Agent   string `json:"agent"`
	Roots   int    `json:"roots"`
	Present int    `json:"present"`
}

type statusService struct {
	Name    string `json:"name"`
	Loaded  *bool  `json:"loaded"`
	Running *bool  `json:"running"`
	Detail  string `json:"detail,omitempty"`
}

type statusTopology struct {
	Mode       string            `json:"mode"`
	StateDir   string            `json:"state_dir"`
	Ambiguity  []string          `json:"ambiguity,omitempty"`
	AgentRoots []statusAgentRoot `json:"agent_roots"`
	Services   []statusService   `json:"services,omitempty"`
}

type statusStorage struct {
	Location       string           `json:"location"`
	Path           *string          `json:"path,omitempty"`
	Files          map[string]int64 `json:"files,omitempty"`
	ObservedWallMS int64            `json:"observed_wall_ms,omitempty"`
	Source         string           `json:"source,omitempty"`
}

type statusSources struct {
	Active  *float64 `json:"active"`
	Stopped *float64 `json:"stopped"`
	Blocked *float64 `json:"blocked"`
	Missing *float64 `json:"missing"`
	Unknown *float64 `json:"unknown"`
}

type statusBacklog struct {
	PendingCompleteBytes *float64           `json:"pending_complete_bytes"`
	PartialTailBytes     *float64           `json:"partial_tail_bytes"`
	UploadBytes          map[string]float64 `json:"upload_bytes,omitempty"`
}

type statusInstance struct {
	InstanceID    string                           `json:"instance_id"`
	PID           int                              `json:"pid"`
	Roles         []string                         `json:"roles"`
	StartWallMS   int64                            `json:"start_wall_ms,omitempty"`
	BuildRevision string                           `json:"build_revision,omitempty"`
	BuildDirty    bool                             `json:"build_dirty,omitempty"`
	Scope         string                           `json:"scope,omitempty"`
	UptimeMS      *int64                           `json:"uptime_ms"`
	SampledWallMS *int64                           `json:"sampled_wall_ms"`
	SampleAgeMS   *int64                           `json:"sample_age_ms"`
	Fresh         bool                             `json:"fresh"`
	Process       string                           `json:"process"`
	ProcessDetail string                           `json:"process_detail,omitempty"`
	Sources       statusSources                    `json:"sources"`
	Excluded      map[string]float64               `json:"excluded,omitempty"`
	Backlog       *statusBacklog                   `json:"backlog,omitempty"`
	LastScanMS    *float64                         `json:"last_scan_ms"`
	Counters      map[string]uint64                `json:"counters,omitempty"`
	Gauges        map[string]float64               `json:"gauges,omitempty"`
	Durations     map[string]observe.HistogramData `json:"durations,omitempty"`
	Resources     map[string]observe.Reading       `json:"resources,omitempty"`
	Truncated     bool                             `json:"truncated,omitempty"`
	Stopped       bool                             `json:"stopped,omitempty"`
	Retained      bool                             `json:"retained,omitempty"`
	DecodeError   string                           `json:"decode_error,omitempty"`
}

type statusRetained struct {
	Total  int            `json:"total"`
	ByRole map[string]int `json:"by_role,omitempty"`
}

type statusFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Evidence string `json:"evidence"`
	Hint     string `json:"hint,omitempty"`
}

type statusOtherState struct {
	StateDir     string   `json:"state_dir"`
	Mode         string   `json:"mode"`
	InstanceIDs  []string `json:"instance_ids,omitempty"`
	InstanceNote string   `json:"instance_note,omitempty"`
}

type statusCoverage struct {
	Present            []string `json:"present,omitempty"`
	Missing            []string `json:"missing,omitempty"`
	Stale              []string `json:"stale,omitempty"`
	Unsupported        []string `json:"unsupported,omitempty"`
	TruncatedDiscovery bool     `json:"truncated_discovery"`
	Notes              []string `json:"notes,omitempty"`
}

type statusRemoteHub struct {
	Reachable             bool                       `json:"reachable"`
	Supported             bool                       `json:"supported"`
	TransportPath         string                     `json:"transport_path,omitempty"`
	RelayRegion           string                     `json:"relay_region,omitempty"`
	Hub                   *hubsync.RemoteIdentity    `json:"hub,omitempty"`
	GeneratedWallMS       int64                      `json:"generated_wall_ms,omitempty"`
	UptimeMS              *int64                     `json:"uptime_ms,omitempty"`
	SampledWallMS         *int64                     `json:"sampled_wall_ms,omitempty"`
	Roles                 []string                   `json:"roles,omitempty"`
	StorageFiles          map[string]int64           `json:"storage_files,omitempty"`
	StorageObservedWallMS int64                      `json:"storage_observed_wall_ms,omitempty"`
	Resources             map[string]observe.Reading `json:"resources,omitempty"`
	Limits                *hubsync.RemoteLimits      `json:"limits,omitempty"`
	Error                 string                     `json:"error,omitempty"`
}

type statusData struct {
	FormatVersion   int                `json:"format_version"`
	GeneratedWallMS int64              `json:"generated_wall_ms"`
	GeneratedUTC    string             `json:"generated_utc"`
	CLI             statusIdentity     `json:"cli"`
	Topology        statusTopology     `json:"topology"`
	Storage         statusStorage      `json:"storage"`
	RemoteHub       *statusRemoteHub   `json:"remote_hub,omitempty"`
	Instances       []statusInstance   `json:"instances"`
	Retained        *statusRetained    `json:"retained,omitempty"`
	OtherStates     []statusOtherState `json:"other_states,omitempty"`
	Findings        []statusFinding    `json:"findings"`
	Coverage        statusCoverage     `json:"coverage"`
}

type remoteStatusFetchResult struct {
	Status        hubsync.RemoteStatus
	TransportPath string
	RelayRegion   string
}

type statusOptions struct {
	remote      bool
	all         bool
	dbPath      string
	stateDir    string
	setupState  string
	role        string
	instanceID  string
	home        string
	nowMS       func() int64
	probePID    func(pid int) string
	loadedRun   func(context.Context, []string) error
	checkRun    func(context.Context, []string) error
	remoteFetch func(context.Context, string) (remoteStatusFetchResult, error)
}

func runStatusCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(statusSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` used to resolve the default state directory")
	state := c.fs.String("state", "",
		"state `directory` whose observe/ tree holds published snapshots; defaults to the database directory")
	role := c.fs.String("role", "",
		"restrict the report to instances carrying this `role` (serve, join, ingest, mcp)")
	instance := c.fs.String("instance", "",
		"restrict the report to one instance `id`")
	remote := c.fs.Bool("remote", false,
		"also fetch cached hub aggregate status over a short-lived authenticated connection")
	all := c.fs.Bool("all", false,
		"include retained terminated instances in the report")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of the text report (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if *role != "" {
		if _, ok := observe.ParseRole(*role); !ok {
			return c.usageErrorf("--role must be one of serve, join, ingest, mcp")
		}
	}
	stateDir, setupState, err := resolveObserveStateDir(*state, *db)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, statusSpec.name, err)
	}
	o := statusOptions{
		remote:      *remote,
		all:         *all,
		dbPath:      *db,
		stateDir:    stateDir,
		setupState:  setupState,
		role:        *role,
		instanceID:  *instance,
		home:        home,
		nowMS:       func() int64 { return time.Now().UnixMilli() },
		probePID:    observe.ProbePID,
		loadedRun:   setup.RunServiceLoaded,
		checkRun:    setup.RunServiceCommand,
		remoteFetch: fetchRemoteHubStatus,
	}
	data, err := buildStatus(context.Background(), o)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, statusSpec.name, err)
	}
	if *asJSON {
		if err := printJSON(stdout, statusSpec.name, data); err != nil {
			return err
		}
	} else {
		writeStatusText(stdout, data)
	}
	if statusDegraded(data) {
		return errDegraded
	}
	return nil
}

func statusDegraded(data statusData) bool {
	for _, f := range data.Findings {
		if f.Severity == sevWarn || f.Severity == sevError {
			return true
		}
	}
	return false
}

func resolveObserveStateDir(explicitState, dbPath string) (stateDir, setupState string, err error) {
	stateDir = explicitState
	setupState = stateDir
	if stateDir == "" {
		setupState = filepath.Dir(dbPath)
	}
	connection, err := setup.LoadConnection(setupState)
	if err != nil {
		return "", "", err
	}
	if stateDir == "" && connection.Mode == "remote" {
		stateDir = filepath.Join(setupState, "join")
	}
	if stateDir == "" {
		stateDir = setupState
	}
	return stateDir, setupState, nil
}

func buildStatus(ctx context.Context, o statusOptions) (statusData, error) {
	nowMS := o.nowMS()
	connection, err := setup.LoadConnection(o.setupState)
	if err != nil {
		return statusData{}, err
	}
	data := statusData{
		FormatVersion:   statusFormatVersion,
		GeneratedWallMS: nowMS,
		GeneratedUTC:    time.UnixMilli(nowMS).UTC().Format(time.RFC3339),
		CLI:             statusCLIIdentity(),
		Topology: statusTopology{
			Mode:     connection.Mode,
			StateDir: o.stateDir,
		},
		Instances: []statusInstance{},
		Findings:  []statusFinding{},
	}
	if o.role != "" {
		data.Coverage.Notes = append(data.Coverage.Notes, "filtered to role "+o.role)
	}
	if o.instanceID != "" {
		data.Coverage.Notes = append(data.Coverage.Notes, "filtered to instance "+o.instanceID)
	}
	data.Topology.AgentRoots = statusAgentRoots(o.home)
	data.Topology.Services = statusServices(ctx, o)
	joined := connection.Mode == "remote"
	data.Storage = statusStorageSection(o, nowMS, joined)

	views, truncated, err := observe.DiscoverInstances(filepath.Join(o.stateDir, "observe"))
	if err != nil {
		return statusData{}, err
	}
	if truncated {
		data.Coverage.TruncatedDiscovery = true
		data.Findings = append(data.Findings, statusFinding{
			Code: "discovery_truncated", Severity: sevWarn,
			Evidence: fmt.Sprintf("more than %d live instances publish under %s", observe.MaxLiveInstances, o.stateDir),
			Hint:     "pass --instance or --role to select one instance explicitly",
		})
	}
	if o.instanceID != "" {
		view, err := observe.LoadInstanceView(filepath.Join(o.stateDir, "observe"), o.instanceID)
		if err != nil {
			return statusData{}, err
		}
		views = []observe.InstanceView{view}
	}
	allInstances := make([]statusInstance, 0, len(views))
	liveByRole := map[string][]string{}
	for _, view := range views {
		if o.role != "" && !view.HasRole(o.role) {
			continue
		}
		inst := buildStatusInstance(view, nowMS, o.probePID)
		allInstances = append(allInstances, inst)
		if inst.DecodeError == "" && inst.SampledWallMS != nil && inst.Process == "running" {
			for _, role := range inst.Roles {
				liveByRole[role] = append(liveByRole[role], inst.InstanceID)
			}
		}
	}
	retained := &statusRetained{ByRole: map[string]int{}}
	var retainedInstances []statusInstance
	unreplacedDeadSeen := map[string]bool{}
	primaryCount := 0
	for _, inst := range allInstances {
		if o.instanceID != "" || !isTerminatedStatus(inst) {
			data.Instances = append(data.Instances, inst)
			primaryCount++
			continue
		}
		if isRetainedStatusInstance(inst, liveByRole, data.Topology.Services, unreplacedDeadSeen) {
			retained.Total++
			retained.ByRole[firstRoleName(inst.Roles)]++
			if o.all {
				inst.Retained = true
				retainedInstances = append(retainedInstances, inst)
			}
			continue
		}
		for _, r := range inst.Roles {
			unreplacedDeadSeen[r] = true
		}
		data.Instances = append(data.Instances, inst)
		primaryCount++
	}
	if o.all && len(retainedInstances) > 0 {
		data.Instances = append(data.Instances, retainedInstances...)
	}
	if retained.Total > 0 {
		data.Retained = retained
	}
	for _, role := range sortedKeys(liveByRole) {
		ids := liveByRole[role]
		if len(ids) > 1 && role != string(observe.RoleMCP) {
			data.Findings = append(data.Findings, statusFinding{
				Code: "multiple_instances", Severity: sevWarn,
				Evidence: fmt.Sprintf("%d live instances with role %s publish in this state directory: %s", len(ids), role, shortIDs(ids)),
				Hint:     "each process is reported separately and their counters are never merged; pass --instance to select one",
			})
		}
		data.Coverage.Present = append(data.Coverage.Present, role)
	}
	sortStrings(data.Coverage.Present)
	data.Findings = append(data.Findings, instanceFindings(data.Instances, nowMS)...)
	data.Storage = mergeStorageFromInstances(data.Storage, data.Instances)
	data.OtherStates = statusOtherStates(o, data.Topology.Mode)
	if joined {
		data.Coverage.Notes = append(data.Coverage.Notes, "joined device: hub-side aggregates are outside this build's local visibility")
		if _, err := os.Stat(o.dbPath); err == nil && !sameDir(filepath.Dir(o.dbPath), o.setupState) {
			data.Topology.Ambiguity = append(data.Topology.Ambiguity,
				fmt.Sprintf("a local database exists at %s while setup/connection.json selects remote memory; the hub owns the live data and this file is not it", o.dbPath))
		}
	} else if hubReceipt(o.setupState) {
		if _, err := os.Stat(filepath.Join(o.setupState, "join", "observe")); err == nil {
			data.Topology.Ambiguity = append(data.Topology.Ambiguity,
				"this state directory also holds a hub service receipt; the hub's devices publish under its own join state directories")
		}
	}
	if o.remote {
		fetch := o.remoteFetch
		if fetch == nil {
			fetch = fetchRemoteHubStatus
		}
		joinState := resolveRemoteStatusJoinState(o)
		res, err := fetch(ctx, joinState)
		switch {
		case errors.Is(err, hubsync.ErrRemoteStatusUnsupported):
			data.RemoteHub = &statusRemoteHub{
				Reachable:     true,
				Supported:     false,
				TransportPath: res.TransportPath,
				RelayRegion:   res.RelayRegion,
			}
			data.Coverage.Unsupported = append(data.Coverage.Unsupported, "remote_hub_aggregates")
			data.Findings = append(data.Findings, statusFinding{
				Code: "remote_status_unsupported", Severity: sevWarn,
				Evidence: "the hub answered but does not advertise remote status aggregates",
				Hint:     "upgrade ohya on the hub host to inspect hub aggregates remotely",
			})
		case err != nil:
			data.RemoteHub = &statusRemoteHub{
				Reachable: false,
				Supported: false,
				Error:     err.Error(),
			}
			data.Findings = append(data.Findings, statusFinding{
				Code: "remote_status_failed", Severity: sevWarn,
				Evidence: "remote hub status fetch failed: " + err.Error(),
				Hint:     "check that ohya serve runs on the hub and this device is paired (run ohya doctor --state " + joinState + ")",
			})
		default:
			hubCopy := res.Status.Hub
			limitsCopy := res.Status.Limits
			data.RemoteHub = &statusRemoteHub{
				Reachable:             true,
				Supported:             true,
				TransportPath:         res.TransportPath,
				RelayRegion:           res.RelayRegion,
				Hub:                   &hubCopy,
				GeneratedWallMS:       res.Status.GeneratedWallMS,
				UptimeMS:              res.Status.UptimeMS,
				SampledWallMS:         res.Status.SampledWallMS,
				Roles:                 res.Status.Roles,
				StorageFiles:          res.Status.StorageFiles,
				StorageObservedWallMS: res.Status.StorageObservedWallMS,
				Resources:             res.Status.Resources,
				Limits:                &limitsCopy,
			}
			data.Coverage.Present = append(data.Coverage.Present, "remote_hub")
			sortStrings(data.Coverage.Present)
		}
	}
	if primaryCount == 0 && o.instanceID == "" {
		data.Coverage.Missing = append(data.Coverage.Missing, "instances")
		if len(data.Topology.Services) == 0 {
			data.Findings = append(data.Findings, statusFinding{
				Code: "not_set_up", Severity: sevWarn,
				Evidence: fmt.Sprintf("no ohya service is set up for %s and no instance runs", o.stateDir),
				Hint:     "run ohya setup to start capturing sessions on this machine, or ohya setup --join-code-env NAME to join a hub",
			})
		}
		data.Findings = append(data.Findings, statusFinding{
			Code: "no_instances", Severity: sevWarn,
			Evidence: fmt.Sprintf("no instance publishes snapshots under %s", filepath.Join(o.stateDir, "observe")),
			Hint:     "no long-running ohya process may be running in this state directory, or one runs with --observe off; snapshots also appear a moment after startup",
		})
	} else if !statusDegraded(data) {
		data.Findings = append(data.Findings, statusFinding{
			Code: "observation_current", Severity: sevOK,
			Evidence: "every selected instance published a fresh snapshot and no degraded component was found",
		})
	}
	return data, nil
}

func isTerminatedStatus(inst statusInstance) bool {
	return inst.DecodeError == "" && inst.SampledWallMS != nil && (inst.Process == "dead" || inst.Process == "stopped")
}

func isEphemeralRoles(roles []string) bool {
	if len(roles) == 0 {
		return false
	}
	for _, r := range roles {
		if r != string(observe.RoleMCP) {
			return false
		}
	}
	return true
}

func allRolesCovered(roles []string, liveByRole map[string][]string) bool {
	if len(roles) == 0 {
		return false
	}
	for _, r := range roles {
		if len(liveByRole[r]) == 0 {
			return false
		}
	}
	return true
}

func allRolesMarked(roles []string, seen map[string]bool) bool {
	if len(roles) == 0 {
		return false
	}
	for _, r := range roles {
		if !seen[r] {
			return false
		}
	}
	return true
}

func hasLiveDaemon(liveByRole map[string][]string) bool {
	for role, ids := range liveByRole {
		if role != string(observe.RoleMCP) && len(ids) > 0 {
			return true
		}
	}
	return false
}

func hasConfiguredServiceForRoles(roles []string, services []statusService) bool {
	for _, r := range roles {
		for _, svc := range services {
			if (r == string(observe.RoleServe) && svc.Name == "hub-service") ||
				((r == string(observe.RoleIngest) || r == string(observe.RoleJoin)) && svc.Name == "service") {
				return true
			}
		}
	}
	return false
}

func isRetainedStatusInstance(inst statusInstance, liveByRole map[string][]string, services []statusService, unreplacedDeadSeen map[string]bool) bool {
	if isEphemeralRoles(inst.Roles) {
		return true
	}
	if allRolesCovered(inst.Roles, liveByRole) {
		return true
	}
	if inst.Stopped && hasLiveDaemon(liveByRole) && !hasConfiguredServiceForRoles(inst.Roles, services) {
		return true
	}
	return allRolesMarked(inst.Roles, unreplacedDeadSeen)
}

func firstRoleName(roles []string) string {
	if len(roles) == 0 {
		return "unknown"
	}
	return roles[0]
}

func statusCLIIdentity() statusIdentity {
	version, revision, dirty, goVer := buildinfo.Identity()
	schema, err := store.SchemaVersion()
	if err != nil {
		schema = -1
	}
	return statusIdentity{
		Version:  version,
		Revision: revision,
		Dirty:    dirty,
		Go:       goVer,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Schema:   schema,
		Parsers: map[string]int{
			claudecode.AgentName:  claudecode.OutputVersion,
			codex.AgentName:       codex.OutputVersion,
			pi.AgentName:          pi.OutputVersion,
			antigravity.AgentName: antigravity.OutputVersion,
		},
	}
}

func statusAgentRoots(home string) []statusAgentRoot {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	flags := newAgentRootFlags(fs, home)
	roots := flags.roots(fs)
	out := make([]statusAgentRoot, 0, len(roots))
	for _, root := range roots {
		entry := statusAgentRoot{Agent: root.agent, Roots: len(root.dirs)}
		for _, d := range root.dirs {
			if _, err := os.Stat(d.dir); err == nil {
				entry.Present++
			}
		}
		out = append(out, entry)
	}
	return out
}

func statusServices(ctx context.Context, o statusOptions) []statusService {
	var out []statusService
	for _, name := range []string{"service.json", "hub-service.json"} {
		receipt := filepath.Join(o.setupState, "setup", name)
		raw, err := os.ReadFile(receipt)
		if err != nil {
			continue
		}
		var header struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &header)
		platform := "linux"
		if filepath.Ext(header.Path) == ".plist" {
			platform = "darwin"
		}
		label := strings.TrimSuffix(name, filepath.Ext(name))
		entry := statusService{Name: label}
		plan := setup.ServicePlan
		if label == "hub-service" {
			plan = setup.HubServicePlan
		}
		service, err := plan(platform, "/", "/", "/ohya", nil)
		if err != nil {
			entry.Detail = err.Error()
			out = append(out, entry)
			continue
		}
		loaded := o.loadedRun(ctx, service.Loaded) == nil
		running := false
		if loaded {
			running = o.checkRun(ctx, service.Check) == nil
		}
		entry.Loaded = &loaded
		entry.Running = &running
		if !loaded {
			entry.Detail = "the service manager reports the unit as not loaded"
		} else if !running {
			entry.Detail = "the service is loaded but its check command reports no running process"
		}
		out = append(out, entry)
	}
	return out
}

func statusStorageSection(o statusOptions, nowMS int64, joined bool) statusStorage {
	storage := statusStorage{Location: "unknown"}
	if joined {
		storage.Location = "remote-hub"
		storage.Source = "joined device: hub storage sizes live on the hub and are not reported locally"
		return storage
	}
	storage.Location = "local"
	path := o.dbPath
	storage.Path = &path
	storage.ObservedWallMS = nowMS
	storage.Source = "stat"
	storage.Files = map[string]int64{}
	for label, suffix := range map[string]string{"db": "", "wal": "-wal", "shm": "-shm"} {
		if info, err := os.Stat(o.dbPath + suffix); err == nil && info.Mode().IsRegular() {
			storage.Files[label] = info.Size()
		}
	}
	return storage
}

func mergeStorageFromInstances(storage statusStorage, instances []statusInstance) statusStorage {
	if storage.Location != "local" {
		return storage
	}
	for _, instance := range instances {
		if instance.DecodeError != "" || !instance.Fresh || instance.SampledWallMS == nil {
			continue
		}
		files := map[string]int64{}
		for label, value := range instance.Gauges {
			if after, ok := strings.CutPrefix(label, "sqlite_file_bytes{file="); ok {
				files[strings.TrimSuffix(after, "}")] = int64(value)
			}
		}
		if len(files) == 0 {
			continue
		}
		storage.Files = files
		storage.ObservedWallMS = *instance.SampledWallMS
		storage.Source = "published gauge from instance " + shortID(instance.InstanceID)
		break
	}
	return storage
}

func statusOtherStates(o statusOptions, mode string) []statusOtherState {
	var out []statusOtherState
	if mode == "remote" {
		if views, truncated, err := observe.DiscoverInstances(filepath.Join(o.setupState, "observe")); err == nil && (len(views) > 0 || truncated) {
			entry := statusOtherState{StateDir: o.setupState, Mode: "local-hub-candidates"}
			for _, view := range views {
				entry.InstanceIDs = append(entry.InstanceIDs, view.InstanceID)
			}
			entry.InstanceNote = "counters are not merged into the primary report"
			if truncated {
				entry.InstanceNote = fmt.Sprintf("more than %d instances; discovery truncated; ", observe.MaxLiveInstances) + entry.InstanceNote
			}
			out = append(out, entry)
		}
	}
	joinState := filepath.Join(o.setupState, "join")
	if mode == "local" && !sameDir(joinState, o.stateDir) {
		if views, _, err := observe.DiscoverInstances(filepath.Join(joinState, "observe")); err == nil && len(views) > 0 {
			entry := statusOtherState{StateDir: joinState, Mode: "joined-device-candidates"}
			for _, view := range views {
				entry.InstanceIDs = append(entry.InstanceIDs, view.InstanceID)
			}
			entry.InstanceNote = "counters are not merged into the primary report"
			out = append(out, entry)
		}
	}
	return out
}

func buildStatusInstance(view observe.InstanceView, nowMS int64, probe func(int) string) statusInstance {
	instance := statusInstance{
		InstanceID:  view.InstanceID,
		DecodeError: view.DecodeErr,
	}
	snapshot := view.Snapshot
	if snapshot == nil {
		return instance
	}
	instance.PID = snapshot.Instance.PID
	instance.Roles = snapshot.Instance.Roles
	instance.StartWallMS = snapshot.Instance.StartWallMS
	instance.BuildRevision = snapshot.Instance.BuildRevision
	instance.BuildDirty = snapshot.Instance.BuildDirty
	instance.Scope = snapshot.Instance.Scope
	instance.UptimeMS = &snapshot.UptimeMS
	instance.SampledWallMS = &snapshot.SampledWallMS
	age := nowMS - snapshot.SampledWallMS
	instance.SampleAgeMS = &age
	instance.Fresh = age >= 0 && age <= observe.SnapshotStaleAge.Milliseconds()
	instance.Stopped = snapshot.Stopped
	instance.Counters = snapshot.Counters
	instance.Gauges = snapshot.Gauges
	instance.Durations = snapshot.Durations
	instance.Resources = snapshot.Resources
	instance.Truncated = snapshot.Truncated
	if snapshot.Stopped {
		instance.Process = "stopped"
		instance.ProcessDetail = "process shut down cleanly"
	} else {
		instance.Process = probe(snapshot.Instance.PID)
		if instance.Process == "running" && !instance.Fresh {
			instance.Process = "unknown"
			instance.ProcessDetail = "a process holds the pid but the newest snapshot is stale: stalled process or pid reuse"
		}
		if instance.Process == "dead" {
			instance.ProcessDetail = "no process holds the pid of the newest snapshot"
		}
	}
	instance.Sources = statusSourcesFromGauges(snapshot.Gauges)
	instance.Excluded = statusExcludedFromGauges(snapshot.Gauges)
	instance.Backlog = statusBacklogFromGauges(snapshot.Gauges)
	if scan, ok := snapshot.Gauges["discovery_last_scan_ms"]; ok {
		instance.LastScanMS = &scan
	}
	return instance
}

func statusSourcesFromGauges(gauges map[string]float64) statusSources {
	pick := func(state string) *float64 {
		value, ok := gauges["watch_sources{state="+state+"}"]
		if !ok {
			return nil
		}
		return &value
	}
	return statusSources{
		Active:  pick("active"),
		Stopped: pick("stopped"),
		Blocked: pick("blocked"),
		Missing: pick("missing"),
		Unknown: pick("unknown"),
	}
}

func statusExcludedFromGauges(gauges map[string]float64) map[string]float64 {
	var excluded map[string]float64
	for key, value := range gauges {
		if after, ok := strings.CutPrefix(key, "discovery_excluded{agent="); ok {
			if excluded == nil {
				excluded = map[string]float64{}
			}
			excluded[strings.TrimSuffix(after, "}")] = value
		}
	}
	return excluded
}

func statusBacklogFromGauges(gauges map[string]float64) *statusBacklog {
	backlog := &statusBacklog{}
	pending, hasPending := gauges["watch_pending_complete_bytes"]
	tail, hasTail := gauges["watch_partial_tail_bytes"]
	if !hasPending && !hasTail {
		return nil
	}
	if hasPending {
		backlog.PendingCompleteBytes = &pending
	}
	if hasTail {
		backlog.PartialTailBytes = &tail
	}
	for key, value := range gauges {
		if after, ok := strings.CutPrefix(key, "upload_backlog_bytes{agent="); ok {
			if backlog.UploadBytes == nil {
				backlog.UploadBytes = map[string]float64{}
			}
			backlog.UploadBytes[strings.TrimSuffix(after, "}")] = value
		}
	}
	return backlog
}

func instanceFindings(instances []statusInstance, nowMS int64) []statusFinding {
	var out []statusFinding
	for _, instance := range instances {
		if instance.Retained {
			continue
		}
		switch {
		case instance.DecodeError != "":
			severity := sevWarn
			code := "instance_unreadable"
			if strings.Contains(instance.DecodeError, "unsupported") {
				code = "instance_unsupported_version"
				severity = sevError
			}
			out = append(out, statusFinding{
				Code: code, Severity: severity,
				Evidence: fmt.Sprintf("instance %s: %s", shortID(instance.InstanceID), instance.DecodeError),
				Hint:     "a newer or broken build wrote this snapshot; remove the directory if it belongs to a terminated process",
			})
		case instance.SampledWallMS == nil:
			out = append(out, statusFinding{
				Code: "instance_no_snapshot", Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s published no snapshot yet", shortID(instance.InstanceID)),
				Hint:     "the first snapshot appears at startup; a directory that never gains one belongs to a crashed or foreign process",
			})
		case instance.Process == "dead" || instance.Process == "stopped":
			age := "unknown age"
			if instance.SampleAgeMS != nil {
				age = fmt.Sprintf("%s old", ageText(*instance.SampleAgeMS))
			}
			verb := "has no running process"
			if instance.Stopped {
				verb = "stopped cleanly"
			}
			out = append(out, statusFinding{
				Code: "instance_terminated", Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s (pid %d, roles %s) %s; its %s snapshot is the last one published",
					shortID(instance.InstanceID), instance.PID, strings.Join(instance.Roles, ","), verb, age),
				Hint: "start or restart the process to resume publication; a terminated instance is cleaned up after retention",
			})
		case !instance.Fresh:
			age := "unknown age"
			if instance.SampleAgeMS != nil {
				age = fmt.Sprintf("%s old", ageText(*instance.SampleAgeMS))
			}
			out = append(out, statusFinding{
				Code: "instance_stale", Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s (pid %d, roles %s) has a %s snapshot and process state %s",
					shortID(instance.InstanceID), instance.PID, strings.Join(instance.Roles, ","), age, instance.Process),
				Hint: "start or restart the process to resume publication; a terminated instance is cleaned up after retention",
			})
		}
		if instance.Sources.Stopped != nil && *instance.Sources.Stopped > 0 {
			out = append(out, statusFinding{
				Code: "sources_stopped", Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s observes %.0f stopped sources", shortID(instance.InstanceID), *instance.Sources.Stopped),
				Hint:     "run ohya doctor --state " + instance.Scope + " for the stopped-source offset and remedy",
			})
		}
		if instance.Backlog != nil && instance.Backlog.PendingCompleteBytes != nil && *instance.Backlog.PendingCompleteBytes > 0 {
			out = append(out, statusFinding{
				Code: "pending_backlog", Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s has %s of complete records past the acknowledged cursor",
					shortID(instance.InstanceID), byteText(*instance.Backlog.PendingCompleteBytes)),
				Hint: "ingestion or upload has not caught up; compare against upload_backlog_bytes rates with ohya metrics",
			})
		}
		for agent, bytes := range safeUploadBytes(instance) {
			if bytes > 0 {
				out = append(out, statusFinding{
					Code: "upload_backlog", Severity: sevWarn,
					Evidence: fmt.Sprintf("instance %s still has %s of %s records to upload",
						shortID(instance.InstanceID), byteText(bytes), agent),
					Hint: "the uploader retries after reconnects; ohya metrics --window 5m shows the drain rate",
				})
			}
		}
	}
	return out
}

func safeUploadBytes(instance statusInstance) map[string]float64 {
	if instance.Backlog == nil {
		return nil
	}
	return instance.Backlog.UploadBytes
}

func writeStatusText(w io.Writer, data statusData) {
	s := styleFor(w)
	sep := " " + s.bullet() + " "
	rev := data.CLI.Revision
	if data.CLI.Dirty == "true" {
		rev += "-dirty"
	}
	fmt.Fprintln(w, s.joinFit([]string{s.bold("ohya status"), s.dim(data.Topology.Mode), s.dim(fmt.Sprintf("%s (%s)", data.CLI.Version, rev)), s.dim(fmt.Sprintf("schema %d", data.CLI.Schema))}, sep, 2))

	var meta [][2]string
	meta = append(meta, [2]string{"State", data.Topology.StateDir})
	meta = append(meta, [2]string{"Storage", formatStatusStorage(s, data)})
	if data.RemoteHub != nil {
		meta = append(meta, formatRemoteHubRows(s, *data.RemoteHub)...)
	}
	if len(data.Topology.Services) > 0 {
		parts := make([]string, 0, len(data.Topology.Services))
		for _, svc := range data.Topology.Services {
			entry := serviceDisplayName(svc.Name) + " " + s.serviceState(svc.Loaded, svc.Running)
			if svc.Detail != "" {
				entry += " " + s.dim("("+svc.Detail+")")
			}
			parts = append(parts, entry)
		}
		meta = append(meta, [2]string{"Services", strings.Join(parts, sep)})
	}
	if len(data.Topology.AgentRoots) > 0 {
		roots := make([]string, 0, len(data.Topology.AgentRoots))
		for _, root := range data.Topology.AgentRoots {
			roots = append(roots, fmt.Sprintf("%s %d/%d", root.Agent, root.Present, root.Roots))
		}
		meta = append(meta, [2]string{"Agent roots", strings.Join(roots, sep)})
	}
	for _, other := range data.OtherStates {
		meta = append(meta, [2]string{"Other state", fmt.Sprintf("%s (%s): %s",
			other.StateDir, other.Mode, strings.Join(shortIDs(other.InstanceIDs), ", "))})
	}
	writeKeyValues(w, s, "", meta)

	var active, retained []statusInstance
	for _, inst := range data.Instances {
		if inst.Retained {
			retained = append(retained, inst)
		} else {
			active = append(active, inst)
		}
	}
	writeStatusInstancesSection(w, s, active, data.Retained, len(retained) > 0)
	if len(retained) > 0 {
		writeRetainedInstancesSection(w, s, retained)
	}
	if data.Coverage.TruncatedDiscovery {
		fmt.Fprintf(w, "\n%s\n", s.yellow("coverage: discovery truncated at the live-instance bound"))
	}
	if len(data.Coverage.Unsupported) > 0 {
		fmt.Fprintf(w, "\n%s\n", s.dim("coverage: unsupported "+strings.Join(data.Coverage.Unsupported, ", ")))
	}
	writeStatusFindingsText(w, s, data.Findings)
}

func formatStatusStorage(s cliStyle, data statusData) string {
	switch data.Storage.Location {
	case "local":
		var parts []string
		for _, label := range []string{"db", "wal", "shm"} {
			if size, ok := data.Storage.Files[label]; ok {
				parts = append(parts, fmt.Sprintf("%s %s", label, byteText(float64(size))))
			}
		}
		if len(parts) == 0 {
			return fmt.Sprintf("%s %s", *data.Storage.Path, s.dim("(no files present)"))
		}
		age := ageText(data.GeneratedWallMS - data.Storage.ObservedWallMS)
		return fmt.Sprintf("%s  %s", strings.Join(parts, ", "), s.dim(fmt.Sprintf("(%s, %s ago)", *data.Storage.Path, age)))
	default:
		return fmt.Sprintf("%s %s", data.Storage.Location, s.dim("— "+data.Storage.Source))
	}
}

func formatRemoteHubRows(s cliStyle, remote statusRemoteHub) [][2]string {
	if !remote.Reachable {
		return [][2]string{{"Remote hub", s.red("unreachable") + " " + s.dim("("+remote.Error+")")}}
	}
	if !remote.Supported {
		return [][2]string{{"Remote hub", s.yellow("reachable") + " " + s.dim("(remote status aggregates unsupported; upgrade hub)")}}
	}
	sep := " " + s.bullet() + " "
	var parts []string
	if remote.Hub != nil {
		parts = append(parts, fmt.Sprintf("version=%s schema=%d", remote.Hub.Version, remote.Hub.Schema))
	}
	if remote.UptimeMS != nil {
		parts = append(parts, "uptime="+ageText(*remote.UptimeMS))
	}
	if remote.TransportPath != "" {
		path := remote.TransportPath
		if remote.RelayRegion != "" {
			path += " (" + remote.RelayRegion + ")"
		}
		parts = append(parts, "path="+path)
	}
	rows := [][2]string{{"Remote hub", strings.Join(parts, sep)}}
	if len(remote.StorageFiles) > 0 {
		var sizes []string
		for _, label := range []string{"db", "wal", "shm"} {
			if size, ok := remote.StorageFiles[label]; ok {
				sizes = append(sizes, fmt.Sprintf("%s %s", label, byteText(float64(size))))
			}
		}
		if len(sizes) > 0 {
			rows = append(rows, [2]string{"Hub storage", strings.Join(sizes, ", ")})
		}
	}
	return rows
}

func writeStatusInstancesSection(w io.Writer, s cliStyle, active []statusInstance, retained *statusRetained, showingAll bool) {
	running, attention := 0, 0
	for _, inst := range active {
		if inst.DecodeError == "" && inst.Process == "running" && inst.Fresh {
			running++
		} else {
			attention++
		}
	}
	title := "Instances"
	switch {
	case len(active) == 0:
		title = "Instances (none)"
	case attention == 0:
		title = fmt.Sprintf("Instances (%d running)", running)
	case running == 0:
		title = fmt.Sprintf("Instances (%d attention)", attention)
	default:
		title = fmt.Sprintf("Instances (%d running, %d attention)", running, attention)
	}
	fmt.Fprintf(w, "\n%s\n", s.section(title))
	if len(active) > 0 {
		tbl := cliTable{
			indent:  "  ",
			headers: []string{"ROLE", "ID", "PID", "UPTIME", "SAMPLE", "STATE", "DETAILS"},
		}
		for _, inst := range active {
			if inst.DecodeError != "" {
				tbl.addRow(s.dim("-"), shortID(inst.InstanceID), "-", "-", "-", s.red("unreadable"), inst.DecodeError)
				continue
			}
			role := strings.Join(inst.Roles, ",")
			sample := "none"
			if inst.SampleAgeMS != nil {
				sample = ageText(*inst.SampleAgeMS) + " ago"
			}
			pid := "-"
			if inst.PID > 0 {
				pid = fmt.Sprintf("%d", inst.PID)
			}
			tbl.addRow(
				s.cyan(role),
				s.bold(shortID(inst.InstanceID)),
				s.dim(pid),
				agePointer(inst.UptimeMS),
				s.dim(sample),
				s.processState(inst.Process, inst.Fresh),
				formatInstanceDetails(s, inst),
			)
		}
		tbl.render(w, s)
	}
	if retained != nil && retained.Total > 0 && !showingAll {
		if len(active) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "  %s\n", s.dim(s.wrapHanging(formatRetainedSummary(retained), 2)))
	}
}

func writeRetainedInstancesSection(w io.Writer, s cliStyle, retained []statusInstance) {
	fmt.Fprintf(w, "\n%s\n", s.section(fmt.Sprintf("Retained (%d)", len(retained))))
	tbl := cliTable{
		indent:  "  ",
		headers: []string{"ROLE", "ID", "PID", "UPTIME", "LAST SAMPLE", "STATE"},
	}
	for _, inst := range retained {
		role := strings.Join(inst.Roles, ",")
		sample := "none"
		if inst.SampleAgeMS != nil {
			sample = ageText(*inst.SampleAgeMS) + " ago"
		}
		tbl.addRow(
			s.dim(role),
			s.dim(shortID(inst.InstanceID)),
			s.dim(fmt.Sprintf("%d", inst.PID)),
			s.dim(agePointer(inst.UptimeMS)),
			s.dim(sample),
			s.processState(inst.Process, inst.Fresh),
		)
	}
	tbl.render(w, s)
}

func formatRetainedSummary(retained *statusRetained) string {
	type roleCount struct {
		role  string
		count int
	}
	counts := make([]roleCount, 0, len(retained.ByRole))
	for role, n := range retained.ByRole {
		counts = append(counts, roleCount{role: role, count: n})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].count != counts[j].count {
			return counts[i].count > counts[j].count
		}
		return counts[i].role < counts[j].role
	})
	parts := make([]string, 0, len(counts))
	for _, rc := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", rc.count, rc.role))
	}
	return fmt.Sprintf("%d retained (%s) — pass --all to list", retained.Total, strings.Join(parts, ", "))
}

func formatInstanceDetails(s cliStyle, instance statusInstance) string {
	var items []string
	sources := instance.Sources
	if sources.Active != nil {
		items = append(items, fmt.Sprintf("%.0f active", *sources.Active))
	}
	if sources.Stopped != nil && *sources.Stopped > 0 {
		items = append(items, s.red(fmt.Sprintf("%.0f stopped", *sources.Stopped)))
	}
	if sources.Blocked != nil && *sources.Blocked > 0 {
		items = append(items, s.yellow(fmt.Sprintf("%.0f blocked", *sources.Blocked)))
	}
	if sources.Missing != nil && *sources.Missing > 0 {
		items = append(items, s.dim(fmt.Sprintf("%.0f missing", *sources.Missing)))
	}
	if sources.Unknown != nil && *sources.Unknown > 0 {
		items = append(items, s.dim(fmt.Sprintf("%.0f unknown", *sources.Unknown)))
	}
	if len(instance.Excluded) > 0 {
		excluded := make([]string, 0, len(instance.Excluded))
		for _, agent := range sortedKeys(instance.Excluded) {
			excluded = append(excluded, fmt.Sprintf("%s=%.0f", agent, instance.Excluded[agent]))
		}
		items = append(items, "excluded "+strings.Join(excluded, " "))
	}
	if instance.Backlog != nil {
		var backlogParts []string
		if instance.Backlog.PendingCompleteBytes != nil && *instance.Backlog.PendingCompleteBytes > 0 {
			backlogParts = append(backlogParts, s.yellow(byteText(*instance.Backlog.PendingCompleteBytes)+" pending"))
		}
		if instance.Backlog.PartialTailBytes != nil && *instance.Backlog.PartialTailBytes > 0 {
			backlogParts = append(backlogParts, s.dim(byteText(*instance.Backlog.PartialTailBytes)+" tail"))
		}
		for _, agent := range sortedKeys(instance.Backlog.UploadBytes) {
			if bytes := instance.Backlog.UploadBytes[agent]; bytes > 0 {
				backlogParts = append(backlogParts, s.yellow(fmt.Sprintf("upload %s %s", agent, byteText(bytes))))
			}
		}
		if len(backlogParts) > 0 {
			items = append(items, strings.Join(backlogParts, ", "))
		} else if instance.Backlog.PendingCompleteBytes != nil && (sources.Stopped == nil || *sources.Stopped == 0) {
			items = append(items, s.green("caught up"))
		}
	}
	return strings.Join(items, " "+s.bullet()+" ")
}

func agePointer(ms *int64) string {
	if ms == nil {
		return "unknown"
	}
	return ageText(*ms)
}

func shortIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = shortID(id)
	}
	return out
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func ageText(ms int64) string {
	if ms < 0 {
		return "unknown"
	}
	seconds := (ms + 999) / 1000
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm%ds", minutes, seconds%60)
	}
	return fmt.Sprintf("%dh%dm", minutes/60, minutes%60)
}

func byteText(bytes float64) string {
	const kiB = 1024
	switch {
	case bytes >= kiB*kiB*kiB*kiB:
		return fmt.Sprintf("%.1f TiB", bytes/(kiB*kiB*kiB*kiB))
	case bytes >= kiB*kiB*kiB:
		return fmt.Sprintf("%.1f GiB", bytes/(kiB*kiB*kiB))
	case bytes >= kiB*kiB:
		return fmt.Sprintf("%.1f MiB", bytes/(kiB*kiB))
	case bytes >= kiB:
		return fmt.Sprintf("%.1f KiB", bytes/kiB)
	default:
		return fmt.Sprintf("%.0f B", bytes)
	}
}

func sameDir(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(absA) == filepath.Clean(absB)
}

func hubReceipt(state string) bool {
	_, err := os.Stat(filepath.Join(state, "setup", "hub-service.json"))
	return err == nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sortStrings(out)
	return out
}

func resolveRemoteStatusJoinState(o statusOptions) string {
	if _, err := os.Stat(filepath.Join(o.stateDir, "credential.json")); err == nil {
		return o.stateDir
	}
	candidate := filepath.Join(o.setupState, "join")
	if _, err := os.Stat(filepath.Join(candidate, "credential.json")); err == nil {
		return candidate
	}
	return o.stateDir
}

func fetchRemoteHubStatus(ctx context.Context, joinState string) (remoteStatusFetchResult, error) {
	cred, err := device.LoadCredential(joinState)
	if errors.Is(err, fs.ErrNotExist) {
		return remoteStatusFetchResult{}, fmt.Errorf("this state directory holds no pairing credential; run ohya join first")
	}
	if err != nil {
		return remoteStatusFetchResult{}, err
	}
	hubAddr, err := device.LoadHubAddress(joinState)
	if errors.Is(err, fs.ErrNotExist) {
		return remoteStatusFetchResult{}, fmt.Errorf("this state directory holds no hub address; run ohya join with an ohya1 code once to save it")
	}
	if err != nil {
		return remoteStatusFetchResult{}, err
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	node := address.NewDevice(address.DeviceOptions{})
	defer func() {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
		_ = node.Drain(drainCtx)
		drainCancel()
		_ = node.Close()
	}()
	dial := func(ctx context.Context) (net.Conn, error) {
		return node.Dial(ctx, hubAddr)
	}
	session := hubsync.NewRemoteSession(dial, cred.Secret, hubsync.RemoteSessionOptions{})
	defer session.Close()

	status, err := session.Diagnostics(dctx)
	res := remoteStatusFetchResult{
		Status:        status,
		TransportPath: "unknown",
	}
	pathCtx, pathCancel := context.WithTimeout(dctx, 2*time.Second)
	defer pathCancel()
	if report, perr := node.PathInfo(pathCtx, hubAddr); perr == nil {
		if report.Path != "" {
			res.TransportPath = report.Path
		}
		res.RelayRegion = report.RegionCode
	}
	if err != nil {
		return res, err
	}
	return res, nil
}
