package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/setup"
	"github.com/nibzard/ohya/internal/store"
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
		"With --remote the command also reports which remote aggregates this build cannot fetch yet. " +
		"Exit codes: 0 for a complete fresh result, 3 for a valid but partial, stale, or degraded result, 1 for failure, 2 for usage.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya status --json", "ohya status --state ~/.ohya/join --role join"},
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
	Backlog       *statusBacklog                   `json:"backlog,omitempty"`
	LastScanMS    *float64                         `json:"last_scan_ms"`
	Counters      map[string]uint64                `json:"counters,omitempty"`
	Gauges        map[string]float64               `json:"gauges,omitempty"`
	Durations     map[string]observe.HistogramData `json:"durations,omitempty"`
	Resources     map[string]observe.Reading       `json:"resources,omitempty"`
	Truncated     bool                             `json:"truncated,omitempty"`
	DecodeError   string                           `json:"decode_error,omitempty"`
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

type statusData struct {
	FormatVersion   int                `json:"format_version"`
	GeneratedWallMS int64              `json:"generated_wall_ms"`
	GeneratedUTC    string             `json:"generated_utc"`
	CLI             statusIdentity     `json:"cli"`
	Topology        statusTopology     `json:"topology"`
	Storage         statusStorage      `json:"storage"`
	Instances       []statusInstance   `json:"instances"`
	OtherStates     []statusOtherState `json:"other_states,omitempty"`
	Findings        []statusFinding    `json:"findings"`
	Coverage        statusCoverage     `json:"coverage"`
}

type statusOptions struct {
	remote     bool
	dbPath     string
	stateDir   string
	setupState string
	role       string
	instanceID string
	home       string
	nowMS      func() int64
	probePID   func(pid int) string
	loadedRun  func(context.Context, []string) error
	checkRun   func(context.Context, []string) error
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
		"also report remote hub aggregates; this build reports its capability gap until plan 001 milestone 5 ships")
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
	stateDir := *state
	setupState := stateDir
	if stateDir == "" {
		setupState = filepath.Dir(*db)
	}
	connection, err := setup.LoadConnection(setupState)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, statusSpec.name, err)
	}
	if stateDir == "" && connection.Mode == "remote" {
		stateDir = filepath.Join(setupState, "join")
	}
	if stateDir == "" {
		stateDir = setupState
	}
	o := statusOptions{
		remote:     *remote,
		dbPath:     *db,
		stateDir:   stateDir,
		setupState: setupState,
		role:       *role,
		instanceID: *instance,
		home:       home,
		nowMS:      func() int64 { return time.Now().UnixMilli() },
		probePID:   observe.ProbePID,
		loadedRun:  setup.RunServiceLoaded,
		checkRun:   setup.RunServiceCommand,
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
	liveByRole := map[string][]string{}
	for _, view := range views {
		if o.role != "" && !view.HasRole(o.role) {
			continue
		}
		data.Instances = append(data.Instances, buildStatusInstance(view, nowMS, o.probePID))
		if view.Snapshot != nil && view.DecodeErr == "" {
			for _, role := range view.Snapshot.Instance.Roles {
				liveByRole[role] = append(liveByRole[role], view.InstanceID)
			}
		}
	}
	for role, ids := range liveByRole {
		if len(ids) > 1 {
			data.Findings = append(data.Findings, statusFinding{
				Code: "multiple_instances", Severity: sevWarn,
				Evidence: fmt.Sprintf("%d instances with role %s publish in this state directory: %s", len(ids), role, shortIDs(ids)),
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
		data.Coverage.Unsupported = append(data.Coverage.Unsupported, "remote_hub_aggregates")
		data.Findings = append(data.Findings, statusFinding{
			Code: "remote_status_unsupported", Severity: sevWarn,
			Evidence: "this build cannot fetch hub-side aggregates over the sync connection yet",
			Hint:     "remote aggregate delivery is plan 001 milestone 5; run ohya status on the hub host until then",
		})
	}
	if len(data.Instances) == 0 && o.instanceID == "" {
		data.Coverage.Missing = append(data.Coverage.Missing, "instances")
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

func statusCLIIdentity() statusIdentity {
	version, revision, dirty, goVer := buildIdentity()
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
			claudecode.AgentName: claudecode.OutputVersion,
			codex.AgentName:      codex.OutputVersion,
			pi.AgentName:         pi.OutputVersion,
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
		if _, err := os.Stat(receipt); err != nil {
			continue
		}
		platform := "linux"
		if filepath.Ext(receipt) == ".plist" {
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
	instance.Process = probe(snapshot.Instance.PID)
	instance.Counters = snapshot.Counters
	instance.Gauges = snapshot.Gauges
	instance.Durations = snapshot.Durations
	instance.Resources = snapshot.Resources
	instance.Truncated = snapshot.Truncated
	if instance.Process == "running" && !instance.Fresh {
		instance.Process = "unknown"
		instance.ProcessDetail = "a process holds the pid but the newest snapshot is stale: stalled process or pid reuse"
	}
	if instance.Process == "dead" {
		instance.ProcessDetail = "no process holds the pid of the newest snapshot"
	}
	instance.Sources = statusSourcesFromGauges(snapshot.Gauges)
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
		case instance.Process == "dead":
			age := "unknown age"
			if instance.SampleAgeMS != nil {
				age = fmt.Sprintf("%s old", ageText(*instance.SampleAgeMS))
			}
			out = append(out, statusFinding{
				Code: "instance_terminated", Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s (pid %d, roles %s) has no running process; its %s snapshot is the last one published",
					shortID(instance.InstanceID), instance.PID, strings.Join(instance.Roles, ","), age),
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
	fmt.Fprintf(w, "ohya status: mode %s, state %s, generated %s\n", data.Topology.Mode, data.Topology.StateDir, data.GeneratedUTC)
	fmt.Fprintf(w, "cli version=%s revision=%s schema=%d\n", data.CLI.Version, data.CLI.Revision, data.CLI.Schema)
	roots := make([]string, 0, len(data.Topology.AgentRoots))
	for _, root := range data.Topology.AgentRoots {
		roots = append(roots, fmt.Sprintf("%s %d/%d present", root.Agent, root.Present, root.Roots))
	}
	if len(roots) > 0 {
		fmt.Fprintf(w, "agent roots: %s\n", strings.Join(roots, ", "))
	}
	for _, service := range data.Topology.Services {
		loaded, running := "unknown", "unknown"
		if service.Loaded != nil {
			loaded = fmt.Sprintf("%t", *service.Loaded)
		}
		if service.Running != nil {
			running = fmt.Sprintf("%t", *service.Running)
		}
		fmt.Fprintf(w, "service %s: loaded=%s running=%s", service.Name, loaded, running)
		if service.Detail != "" {
			fmt.Fprintf(w, " (%s)", service.Detail)
		}
		fmt.Fprintln(w)
	}
	switch data.Storage.Location {
	case "local":
		var parts []string
		for _, label := range []string{"db", "wal", "shm"} {
			if size, ok := data.Storage.Files[label]; ok {
				parts = append(parts, fmt.Sprintf("%s %s", label, byteText(float64(size))))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(w, "storage: local %s (%s, observed %s ago)\n", strings.Join(parts, ", "), *data.Storage.Path, ageText(data.GeneratedWallMS-data.Storage.ObservedWallMS))
		} else {
			fmt.Fprintf(w, "storage: local %s (no files present)\n", *data.Storage.Path)
		}
	default:
		fmt.Fprintf(w, "storage: %s; %s\n", data.Storage.Location, data.Storage.Source)
	}
	for _, instance := range data.Instances {
		writeStatusInstanceText(w, instance)
	}
	for _, other := range data.OtherStates {
		fmt.Fprintf(w, "also publishing under %s (%s): %s\n", other.StateDir, other.Mode, strings.Join(shortIDs(other.InstanceIDs), ", "))
	}
	if data.Coverage.TruncatedDiscovery {
		fmt.Fprintln(w, "coverage: discovery truncated at the live-instance bound")
	}
	if len(data.Coverage.Unsupported) > 0 {
		fmt.Fprintf(w, "coverage: unsupported %s\n", strings.Join(data.Coverage.Unsupported, ", "))
	}
	for _, finding := range data.Findings {
		line := fmt.Sprintf("%-5s %s: %s", finding.Severity, finding.Code, finding.Evidence)
		if finding.Hint != "" {
			line += "; hint: " + finding.Hint
		}
		fmt.Fprintln(w, line)
	}
}

func writeStatusInstanceText(w io.Writer, instance statusInstance) {
	if instance.DecodeError != "" {
		fmt.Fprintf(w, "instance %s: unreadable snapshot: %s\n", shortID(instance.InstanceID), instance.DecodeError)
		return
	}
	age := "no sample"
	if instance.SampleAgeMS != nil {
		age = ageText(*instance.SampleAgeMS) + " old"
	}
	fmt.Fprintf(w, "instance %s pid %d roles %s uptime %s sample %s (%s) process %s\n",
		shortID(instance.InstanceID), instance.PID, strings.Join(instance.Roles, ","),
		agePointer(instance.UptimeMS), age, freshness(instance.Fresh), instance.Process)
	sources := instance.Sources
	parts := []string{}
	for _, entry := range []struct {
		name  string
		value *float64
	}{
		{"active", sources.Active},
		{"stopped", sources.Stopped},
		{"blocked", sources.Blocked},
		{"missing", sources.Missing},
		{"unknown", sources.Unknown},
	} {
		if entry.value != nil {
			parts = append(parts, fmt.Sprintf("%s=%.0f", entry.name, *entry.value))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "  sources %s\n", strings.Join(parts, " "))
	}
	if instance.Backlog != nil {
		if instance.Backlog.PendingCompleteBytes != nil {
			fmt.Fprintf(w, "  pending complete %s", byteText(*instance.Backlog.PendingCompleteBytes))
			if instance.Backlog.PartialTailBytes != nil {
				fmt.Fprintf(w, ", partial tail %s", byteText(*instance.Backlog.PartialTailBytes))
			}
			fmt.Fprintln(w)
		}
		for _, agent := range sortedKeys(instance.Backlog.UploadBytes) {
			fmt.Fprintf(w, "  upload backlog %s: %s\n", agent, byteText(instance.Backlog.UploadBytes[agent]))
		}
	}
}

func freshness(fresh bool) string {
	if fresh {
		return "fresh"
	}
	return "stale"
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
	case bytes >= 64*kiB*kiB*kiB:
		return fmt.Sprintf("%.1f TiB", bytes/(1024*kiB*kiB*kiB))
	case bytes >= 64*kiB*kiB:
		return fmt.Sprintf("%.1f GiB", bytes/(1024*kiB*kiB))
	case bytes >= 64*kiB:
		return fmt.Sprintf("%.1f MiB", bytes/(1024*kiB))
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
