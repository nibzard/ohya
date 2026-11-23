package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/setup"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/transport/address"
	"github.com/nibzard/ohya/internal/watch"
)

const (
	sevOK    = "ok"
	sevWarn  = "warn"
	sevError = "error"
)

var errDoctorFailed = errors.New("doctor found one or more error findings")

var doctorSpec = commandSpec{
	name:      "ohya doctor",
	synopsis:  "run read-only health checks the sql tool cannot answer",
	usageLine: "ohya doctor [flags]",
	description: "Checks state outside the read-only sql tool's reach: the database schema against this build's migrations, stored parser output versions this build's rederive refuses, the FTS5 integrity of events_fts with --deep, stopped sources in the local watch state (ADR 0004), the device identity, and — when the state directory holds a pairing credential — that the saved hub address is present and readable. A state directory with an owned ohya serve service also reports whether the hub identity exists. A user unit that starts this binary as the hub (ohya serve) has its ExecStart checked against this build's serve flags and its running process checked for a replaced executable: a unit this build cannot start, or a service still running a binary that was replaced on disk, is an error. " +
		"On a paired device, doctor also dials the hub through the tailcat transport and reports whether it answers, the path type (direct or relay), the relay region, the round-trip time, and the compatibility of sync protocol versions; a value the transport library does not report is printed as unknown, and the hub address itself never appears.\n" +
		"Doctor opens the database read-only: it never migrates, repairs, or re-derives, and prints no transcript content or secrets.\n" +
		"The --deep check copies the database into memory, so it takes time and memory in proportion to the database size.\n" +
		"Every finding carries a severity and a one-line remedy. Exit codes: 0 when no finding is an error, 1 otherwise; warnings do not fail. A joined device checks its own state with --state ~/.ohya/join.",
	examples: []string{"ohya doctor", "ohya doctor --state ~/.ohya/join"},
}

type finding struct {
	Check    string `json:"check"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
	Remedy   string `json:"remedy"`
}

type doctorJSONData struct {
	Findings  []finding           `json:"findings"`
	Transport *doctorTransportSum `json:"transport,omitempty"`
}

type doctorTransportSum struct {
	HubAnswers         *bool    `json:"hub_answers"`
	Path               string   `json:"path"`
	RelayRegion        *string  `json:"relay_region"`
	RoundTripMS        *float64 `json:"round_trip_ms"`
	ProtocolVersion    int      `json:"protocol_version"`
	ProtocolCompatible *bool    `json:"protocol_compatible"`
}

const doctorProbeTimeout = 45 * time.Second

func runDoctorCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(doctorSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	state := c.fs.String("state", "",
		"state `directory` to inspect: device identity, pairing credential, source checkpoints; defaults to the database directory")
	maxRecordBytes := c.fs.Int64("max-record-bytes", claudecode.DefaultMaxRecordBytes,
		"record size `bound` the stop check parses with; pass the bound ingestion last ran with (docs/adr/0004)")
	deep := c.fs.Bool("deep", false, "also verify events_fts against the events table; copies the database into memory")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of the text findings (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if *maxRecordBytes <= 0 {
		return c.usageErrorf("--max-record-bytes must be positive")
	}
	explicitState := *state != ""
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	setupState := stateDir
	connection, err := setup.LoadConnection(setupState)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, doctorSpec.name, err)
	}
	explicitDB := false
	c.fs.Visit(func(f *flag.Flag) {
		if f.Name == "db" {
			explicitDB = true
		}
	})
	remote := connection.Mode == "remote" && !explicitDB
	if !explicitState && connection.Mode == "remote" {
		stateDir = filepath.Join(setupState, "join")
	}
	findings, transport, err := runDoctor(context.Background(), doctorOptions{
		remote:         remote,
		dbPath:         *db,
		stateDir:       stateDir,
		maxRecordBytes: *maxRecordBytes,
		deep:           *deep,
	})
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, doctorSpec.name, err)
	}
	if _, err := os.Stat(filepath.Join(setupState, "setup", "hub-service.json")); err == nil {
		findings = append(findings, checkHubIdentity(setupState))
	} else if !os.IsNotExist(err) {
		return jsonFailurePrint(*asJSON, stdout, doctorSpec.name, err)
	}
	inspectRun := func(ctx context.Context, args []string) error { return setup.RunServiceCommand(ctx, args) }
	if run, _ := serviceRunners(); run != nil {
		inspectRun = run
	}
	integrations, inspectErr := setup.Inspect(context.Background(), setupState, inspectRun)
	if inspectErr != nil {
		return jsonFailurePrint(*asJSON, stdout, doctorSpec.name, inspectErr)
	}
	for _, item := range integrations {
		severity := sevOK
		if item.Status == "changed" {
			severity = sevWarn
		}
		if item.Status == "failed" {
			severity = sevError
		}
		detail := item.Detail
		if detail == "" {
			detail = "owned integration is configured"
		}
		f := finding{Check: "setup:" + item.Component, Severity: severity, Detail: detail}
		if severity != sevOK {
			f.Remedy = "rerun ohya setup to repair owned configuration; preserve intentional user edits"
		}
		findings = append(findings, f)
	}
	findings = append(findings, checkHubUnits(context.Background(), home)...)
	if *asJSON {
		if err := printJSON(stdout, doctorSpec.name, doctorJSONData{Findings: findings, Transport: transport}); err != nil {
			return err
		}
	} else {
		writeFindings(stdout, findings)
	}
	if hasError(findings) {
		return errDoctorFailed
	}
	return nil
}

type doctorOptions struct {
	remote         bool
	dbPath         string
	stateDir       string
	maxRecordBytes int64
	deep           bool
	probeTimeout   time.Duration
}

func runDoctor(ctx context.Context, o doctorOptions) ([]finding, *doctorTransportSum, error) {
	findings := make([]finding, 0)
	binSchema, err := store.SchemaVersion()
	if err != nil {
		return nil, nil, err
	}
	var hub *store.DB
	switch _, statErr := os.Stat(o.dbPath); {
	case o.remote:
		findings = append(findings, finding{Check: "database", Severity: sevOK, Detail: "remote memory uses the paired hub; no local database is required"})
	case errors.Is(statErr, fs.ErrNotExist):
		findings = append(findings, finding{
			Check: "database", Severity: sevWarn,
			Detail: fmt.Sprintf("hub database %s does not exist", o.dbPath),
			Remedy: "run ohya ingest --once to create and migrate it",
		})
	case statErr != nil:
		findings = append(findings, finding{
			Check: "database", Severity: sevError,
			Detail: fmt.Sprintf("cannot stat %s: %v", o.dbPath, statErr),
			Remedy: "check the --db path and its permissions",
		})
	default:
		hub, err = store.OpenReadOnly(o.dbPath)
		if err != nil {
			findings = append(findings, finding{
				Check: "database", Severity: sevError,
				Detail: fmt.Sprintf("cannot open %s read-only: %v", o.dbPath, err),
				Remedy: "check the --db path and its permissions; another ohya process may hold the database",
			})
		}
	}
	if hub != nil {
		defer hub.Close()
		findings = append(findings, checkSchema(ctx, hub, o.dbPath, binSchema)...)
		findings = append(findings, checkParserVersions(ctx, hub)...)
		findings = append(findings, checkFTSIndex(ctx, hub, o.deep)...)
	}
	if o.remote {
		findings = append(findings, checkUploadCheckpoints(o.stateDir)...)
	} else {
		findings = append(findings, checkStoppedSources(ctx, hub, o)...)
	}
	findings = append(findings, checkDeviceIdentity(o.stateDir))
	join, joinFindings := checkJoinState(o)
	findings = append(findings, joinFindings...)
	transport := (*doctorTransportSum)(nil)
	if join.paired {
		var tFindings []finding
		tFindings, transport = checkHubTransport(ctx, o, join)
		findings = append(findings, tFindings...)
	}
	return findings, transport, nil
}

func checkSchema(ctx context.Context, hub *store.DB, dbPath string, binSchema int) []finding {
	version, found, err := hub.AppliedSchemaVersion(ctx)
	switch {
	case errors.Is(err, store.ErrNoSchemaMigrations):
		return []finding{{Check: "schema_version", Severity: sevError,
			Detail: fmt.Sprintf("%s holds no schema_migrations table; not an ohya hub database", dbPath),
			Remedy: "check the --db path"}}
	case err != nil:
		return []finding{{Check: "schema_version", Severity: sevError,
			Detail: fmt.Sprintf("cannot read the applied schema version: %v", err)}}
	case !found:
		return []finding{{Check: "schema_version", Severity: sevError,
			Detail: "schema_migrations holds no applied migration",
			Remedy: "run ohya ingest --once to migrate the database"}}
	case version < binSchema:
		return []finding{{Check: "schema_version", Severity: sevWarn,
			Detail: fmt.Sprintf("database schema %d is behind this build's migrations (%d)", version, binSchema),
			Remedy: "run ohya ingest --once; its writer migrates the database (doctor never does)"}}
	case version > binSchema:
		return []finding{{Check: "schema_version", Severity: sevError,
			Detail: fmt.Sprintf("database schema %d is ahead of this build's migrations (%d)", version, binSchema),
			Remedy: "upgrade the ohya binary"}}
	default:
		return []finding{{Check: "schema_version", Severity: sevOK,
			Detail: fmt.Sprintf("database schema %d matches this build's migrations", version)}}
	}
}

func checkParserVersions(ctx context.Context, hub *store.DB) []finding {
	build := map[string]int{}
	for _, p := range events.Derivers() {
		build[p.Agent] = p.OutputVersion
	}
	stored, err := hub.MaxParserVersions(ctx)
	if err != nil {
		return []finding{{Check: "parser_versions", Severity: sevError,
			Detail: fmt.Sprintf("cannot read stored parser output versions: %v", err)}}
	}
	var out []finding
	var current []string
	for _, agent := range slices.Sorted(maps.Keys(stored)) {
		bin, ok := build[agent]
		switch {
		case !ok:
			out = append(out, finding{Check: "parser_versions", Severity: sevError,
				Detail: fmt.Sprintf("agent %s has no parser in this build", agent),
				Remedy: "upgrade the ohya binary"})
		case stored[agent] > bin:
			out = append(out, finding{Check: "parser_versions", Severity: sevError,
				Detail: fmt.Sprintf("agent %s rows carry parser output version %d, above this build's %d; ohya rederive refuses them", agent, stored[agent], bin),
				Remedy: "upgrade the ohya binary, then run ohya rederive"})
		default:
			current = append(current, fmt.Sprintf("%s %d", agent, stored[agent]))
		}
	}
	if len(out) > 0 {
		return out
	}
	detail := "every stored row is at or below this build's parser output versions"
	if len(current) > 0 {
		detail = "stored parser output versions: " + strings.Join(current, ", ")
	}
	return []finding{{Check: "parser_versions", Severity: sevOK, Detail: detail}}
}

func checkFTSIndex(ctx context.Context, hub *store.DB, deep bool) []finding {
	if !deep {
		return []finding{{Check: "fts_index", Severity: sevOK,
			Detail: "events_fts integrity-check skipped; run ohya doctor --deep to verify it"}}
	}
	if err := hub.FTSIndexConsistent(ctx); err != nil {
		return []finding{{Check: "fts_index", Severity: sevError,
			Detail: fmt.Sprintf("events_fts failed the FTS5 integrity-check: %v", err),
			Remedy: "restore the database from a backup; ohya rederive rebuilds events_fts after a parser upgrade"}}
	}
	return []finding{{Check: "fts_index", Severity: sevOK,
		Detail: "events_fts passes the FTS5 integrity-check"}}
}

func checkStoppedSources(ctx context.Context, hub *store.DB, o doctorOptions) []finding {
	var out []finding
	stateLost := false
	watched := 0
	for _, agent := range events.Agents {
		snaps, err := watch.InspectSources(o.stateDir, agent)
		if err != nil {
			stateLost = true
			out = append(out, finding{Check: "stopped_sources", Severity: sevError,
				Detail: fmt.Sprintf("%s checkpoint state in %s cannot be read: %v", agent, o.stateDir, err),
				Remedy: "restore the state directory's checkpoint files from a backup, or delete them to re-import every source under new source IDs (duplicates history)"})
			continue
		}
		watched += len(snaps)
		for _, s := range snaps {
			if f, stopped := stoppedAt(ctx, hub, agent, s, o.maxRecordBytes); stopped {
				out = append(out, f)
			}
		}
	}
	if len(out) == 0 && !stateLost {
		detail := fmt.Sprintf("no source is stopped at its checkpoint (%d watched)", watched)
		if watched == 0 {
			detail = "no watched sources in the state directory yet"
		}
		out = append(out, finding{Check: "stopped_sources", Severity: sevOK, Detail: detail})
	}
	return out
}

func stoppedAt(ctx context.Context, hub *store.DB, agent string, s watch.SourceSnapshot, limit int64) (finding, bool) {
	info, err := os.Stat(s.Path)
	if err != nil || !info.Mode().IsRegular() {
		return finding{}, false
	}
	pending := info.Size() - s.CursorBytes
	if pending <= 0 {
		return finding{}, false
	}
	buf := make([]byte, min(pending, limit+1))
	f, err := os.Open(s.Path)
	if err != nil {
		return finding{}, false
	}
	defer f.Close()
	if _, err := f.ReadAt(buf, s.CursorBytes); err != nil {
		return finding{}, false
	}
	opts := events.Options{SourceScope: s.SourceID, SourcePath: s.Path, MaxRecordBytes: limit}
	if hub != nil && s.CursorBytes > 0 {
		if st, err := hub.ReadOnlySourceState(ctx, s.SourceID); err == nil {
			opts.FirstNative = st.FirstNativeID
		}
	}
	_, perr := events.Parse(agent, buf, s.CursorBytes, opts)
	var stop *events.StopError
	if !errors.As(perr, &stop) || stop.Offset != s.CursorBytes {
		return finding{}, false
	}
	remedy := "repair the record at this offset in the source file; ohya never skips bytes (docs/adr/0004-stopped-source-recovery.md)"
	if stop.RecordTooLarge {
		remedy = "rerun ingest with --max-record-bytes above this record to resume at this offset (docs/adr/0004-stopped-source-recovery.md)"
	}
	return finding{Check: "stopped_sources", Severity: sevWarn,
		Detail: fmt.Sprintf("%s stopped at offset %d: %v", s.Path, s.CursorBytes, stop.Err),
		Remedy: remedy,
	}, true
}

func checkDeviceIdentity(stateDir string) finding {
	id, err := device.Load(stateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return finding{Check: "device_identity", Severity: sevWarn,
			Detail: fmt.Sprintf("no %s in %s", device.FileName, stateDir),
			Remedy: "run ohya ingest --once; it creates the identity on first use"}
	case err != nil:
		return finding{Check: "device_identity", Severity: sevError,
			Detail: err.Error(),
			Remedy: "restore device.json from a backup, or delete it to start over as a new device"}
	default:
		return finding{Check: "device_identity", Severity: sevOK,
			Detail: fmt.Sprintf("%s holds device %s", device.FileName, id)}
	}
}

type doctorJoinState struct {
	paired     bool
	credential device.Credential
	hubAddr    string
}

func checkJoinState(o doctorOptions) (doctorJoinState, []finding) {
	cred, err := device.LoadCredential(o.stateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return doctorJoinState{}, []finding{{Check: "join_state", Severity: sevOK,
			Detail: fmt.Sprintf("no %s in %s; this state directory is not paired with a hub", device.CredentialFileName, o.stateDir)}}
	case err != nil:
		return doctorJoinState{}, []finding{{Check: "join_state", Severity: sevError,
			Detail: err.Error(),
			Remedy: "restore credential.json from a backup, or have the hub revoke this device identity and re-pair with a new one"}}
	}
	hubAddr, err := device.LoadHubAddress(o.stateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return doctorJoinState{}, []finding{{Check: "join_state", Severity: sevWarn,
			Detail: fmt.Sprintf("paired as device %s, but no %s in %s", cred.DeviceID, device.HubAddressFileName, o.stateDir),
			Remedy: "run ohya join with a fresh ohya1 code; a still-valid credential keeps its identity and the invitation stays unused"}}
	case err != nil:
		return doctorJoinState{}, []finding{{Check: "join_state", Severity: sevError,
			Detail: err.Error(),
			Remedy: "run ohya join with a fresh ohya1 code to save the hub's address again"}}
	case !address.ValidHubAddr(hubAddr):
		return doctorJoinState{}, []finding{{Check: "join_state", Severity: sevError,
			Detail: fmt.Sprintf("%s in %s holds no valid tailcat address", device.HubAddressFileName, o.stateDir),
			Remedy: "run ohya join with a fresh ohya1 code to save the hub's address again"}}
	}
	return doctorJoinState{paired: true, credential: cred, hubAddr: hubAddr},
		[]finding{{Check: "join_state", Severity: sevOK,
			Detail: fmt.Sprintf("paired as device %s; %s holds the hub address", cred.DeviceID, device.HubAddressFileName)}}
}

func checkHubTransport(ctx context.Context, o doctorOptions, join doctorJoinState) ([]finding, *doctorTransportSum) {
	timeout := o.probeTimeout
	if timeout <= 0 {
		timeout = doctorProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sum := &doctorTransportSum{Path: address.PathUnknown, ProtocolVersion: hubsync.ProtocolVersion}
	node := address.NewDevice(address.DeviceOptions{})
	defer node.Close()

	report, err := node.PathInfo(pctx, join.hubAddr)
	if err != nil {
		answers := false
		sum.HubAnswers = &answers
		return []finding{{Check: "hub_transport", Severity: sevWarn,
			Detail: fmt.Sprintf("the hub did not answer: %v", err),
			Remedy: "check that ohya serve runs on the hub and this device can reach a relay (outbound HTTPS, TCP 443)"}}, sum
	}
	answers := true
	sum.HubAnswers = &answers
	sum.Path = report.Path
	sum.RoundTripMS = &report.RoundTripMS
	if report.Path == address.PathRelay {
		region := relayRegionText(report.RegionID, report.RegionCode)
		sum.RelayRegion = &region
	}
	detail := fmt.Sprintf("hub answers through a %s path, round trip %.1f ms", report.Path, report.RoundTripMS)
	if sum.RelayRegion != nil {
		detail = fmt.Sprintf("hub answers through a relay path (region %s), round trip %.1f ms", *sum.RelayRegion, report.RoundTripMS)
	}
	findings := []finding{{Check: "hub_transport", Severity: sevOK, Detail: detail}}

	session := hubsync.NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) { return node.Dial(ctx, join.hubAddr) },
		join.credential.Secret, hubsync.RemoteSessionOptions{})
	defer session.Close()
	if _, qerr := session.Query(pctx, "SELECT 1"); qerr == nil {
		compatible := true
		sum.ProtocolCompatible = &compatible
		findings = append(findings, finding{Check: "hub_protocol", Severity: sevOK,
			Detail: fmt.Sprintf("this build's sync protocol %d is compatible with the hub", hubsync.ProtocolVersion)})
		return findings, sum
	} else if isVersionMismatch(qerr) {
		compatible := false
		sum.ProtocolCompatible = &compatible
		findings = append(findings, finding{Check: "hub_protocol", Severity: sevError,
			Detail: fmt.Sprintf("the hub and this device speak different sync protocol versions: this build speaks %d (%v)", hubsync.ProtocolVersion, qerr),
			Remedy: "upgrade ohya on the older machine with: " + upgradeCommand})
	} else if errors.Is(qerr, hubsync.ErrRemoteUnauthorized) {
		findings = append(findings, finding{Check: "hub_protocol", Severity: sevError,
			Detail: fmt.Sprintf("the hub refused this device's credential: %v", qerr),
			Remedy: "the device identity was revoked or the address names a different hub; have the hub revoke this identity and re-pair with a new one"})
	} else {
		findings = append(findings, finding{Check: "hub_protocol", Severity: sevWarn,
			Detail: fmt.Sprintf("cannot check the sync protocol against the hub: %v", qerr),
			Remedy: "check that ohya serve runs on the hub; the versions are unknown until the hub answers"})
	}
	return findings, sum
}

func relayRegionText(id int64, code string) string {
	if id != 0 && code != "" {
		return fmt.Sprintf("%d %s", id, code)
	}
	if code != "" {
		return code
	}
	return fmt.Sprintf("%d", id)
}

func checkHubIdentity(state string) finding {
	info, err := address.LoadHubInfo(state)
	switch {
	case errors.Is(err, address.ErrNoIdentity):
		return finding{Check: "hub_identity", Severity: sevWarn,
			Detail: fmt.Sprintf("the hub service is configured but %s holds no hub identity", filepath.Join(state, "tailcat-hub")),
			Remedy: "the ohya serve service creates the identity on first start; check the service, then rerun ohya setup --hub"}
	case err != nil:
		return finding{Check: "hub_identity", Severity: sevError,
			Detail: err.Error(),
			Remedy: "restore tailcat-hub/identity.json from a backup; a new identity changes the hub address, so devices then need a fresh code"}
	}
	return finding{Check: "hub_identity", Severity: sevOK,
		Detail: fmt.Sprintf("hub identity present (node key %s, relay region %d %s)", info.NodeKey, info.RegionID, info.RegionCode)}
}

var hubUnitMainPID = setup.ServiceMainPID

func serveArgsProblem(args []string) error {
	s := newServeFlagSet("")
	if err := s.c.fs.Parse(args); err != nil {
		return err
	}
	if s.c.fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", s.c.fs.Arg(0))
	}
	if _, err := parseObserveFlag(*s.observe); err != nil {
		return err
	}
	return nil
}

func checkHubUnits(ctx context.Context, home string) []finding {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	units, err := setup.ScanServeUnits(filepath.Join(home, ".config", "systemd", "user"), self)
	if err != nil || len(units) == 0 {
		return nil
	}
	var out []finding
	for _, u := range units {
		out = append(out, checkHubUnit(ctx, u)...)
	}
	return out
}

func checkHubUnit(ctx context.Context, u setup.UnitExecStart) []finding {
	var out []finding
	if problem := serveArgsProblem(u.Args[1:]); problem != nil {
		remedy := fmt.Sprintf("rerun ohya setup --hub to rewrite %s with this build's serve flags and restart the service", u.Unit)
		ownedUnit := "ohya-serve.service"
		if plan, err := setup.HubServicePlan("linux", "/", "/", "/ohya", nil); err == nil {
			ownedUnit = filepath.Base(plan.Path)
		}
		if u.Unit != ownedUnit {
			remedy = fmt.Sprintf("rerun ohya setup --hub to install and start the owned %s, then retire %s: systemctl --user disable --now %s", ownedUnit, u.Unit, u.Unit)
		}
		out = append(out, finding{Check: "hub_unit", Severity: sevError,
			Detail: fmt.Sprintf("%s starts this binary with serve arguments this build rejects: %v; the service cannot start again, so the next restart (reboot, crash, or manual) leaves the hub down", u.Unit, problem),
			Remedy: remedy})
	}
	if f, replaced := checkHubUnitProcess(ctx, u); replaced {
		out = append(out, f)
	}
	if len(out) == 0 {
		out = append(out, finding{Check: "hub_unit", Severity: sevOK,
			Detail: fmt.Sprintf("%s starts this binary with serve arguments this build accepts", u.Unit)})
	}
	return out
}

func checkHubUnitProcess(ctx context.Context, u setup.UnitExecStart) (finding, bool) {
	pid, err := hubUnitMainPID(ctx, u.Unit)
	if err != nil || pid <= 0 {
		return finding{}, false
	}
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil || !strings.HasSuffix(link, " (deleted)") {
		return finding{}, false
	}
	return finding{Check: "hub_unit", Severity: sevError,
		Detail: fmt.Sprintf("%s (main PID %d) still runs an executable that was replaced on disk: %s; the service is not running the installed binary", u.Unit, pid, link),
		Remedy: fmt.Sprintf("run systemctl --user restart %s once the unit starts the installed binary, so the hub runs the installed binary", u.Unit)}, true
}

func writeFindings(w io.Writer, findings []finding) {
	s := styleFor(w)
	if !s.color {
		for _, f := range findings {
			line := fmt.Sprintf("%-5s %s: %s", f.Severity, f.Check, f.Detail)
			if f.Remedy != "" {
				line += "; remedy: " + f.Remedy
			}
			fmt.Fprintln(w, line)
		}
		return
	}
	findings = collapseSetupOK(findings)
	maxCheck := 0
	okCount, warnCount, errCount := 0, 0, 0
	for _, f := range findings {
		if vw := visibleWidth(f.Check); vw > maxCheck {
			maxCheck = vw
		}
		switch f.Severity {
		case sevOK:
			okCount++
		case sevWarn:
			warnCount++
		case sevError:
			errCount++
		}
	}
	fmt.Fprintf(w, "%s\n\n", s.bold("ohya doctor"))
	detailIndent := 2 + 1 + 2 + maxCheck + 2
	for _, f := range findings {
		fmt.Fprintf(w, "  %s  %s  %s\n", s.severityTag(f.Severity), padRight(s.bold(f.Check), maxCheck), s.wrapHanging(f.Detail, detailIndent))
		if f.Remedy != "" {
			fmt.Fprintf(w, "%s%s\n", strings.Repeat(" ", detailIndent), s.dim(s.wrapHanging("→ "+f.Remedy, detailIndent)))
		}
	}
	fmt.Fprintln(w)
	counts := []string{}
	if errCount > 0 {
		counts = append(counts, plural(errCount, "error", "errors"))
	}
	if warnCount > 0 {
		counts = append(counts, plural(warnCount, "warning", "warnings"))
	}
	counts = append(counts, fmt.Sprintf("%d ok", okCount))
	switch {
	case errCount > 0:
		fmt.Fprintf(w, "%s %s\n", s.boldRed("✗"), s.red(strings.Join(counts, ", ")))
	case warnCount > 0:
		fmt.Fprintf(w, "%s %s\n", s.boldYellow("▲"), s.yellow(strings.Join(counts, ", ")))
	default:
		fmt.Fprintf(w, "%s %s\n", s.boldGreen("✓"), s.green(fmt.Sprintf("all %d checks passed", okCount)))
	}
}

func collapseSetupOK(findings []finding) []finding {
	var out []finding
	var components []string
	at := -1
	for _, f := range findings {
		component, isSetup := strings.CutPrefix(f.Check, "setup:")
		if !isSetup || f.Severity != sevOK || f.Detail != "owned integration is configured" {
			out = append(out, f)
			continue
		}
		if at < 0 {
			at = len(out)
			out = append(out, finding{})
		}
		components = append(components, component)
	}
	if at >= 0 {
		out[at] = finding{Check: "setup", Severity: sevOK,
			Detail: fmt.Sprintf("%s configured: %s", plural(len(components), "owned integration", "owned integrations"), strings.Join(components, ", "))}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func hasError(findings []finding) bool {
	for _, f := range findings {
		if f.Severity == sevError {
			return true
		}
	}
	return false
}

func checkUploadCheckpoints(state string) []finding {
	count := 0
	var out []finding
	for _, agent := range events.Agents {
		snapshots, err := watch.InspectSources(state, agent)
		if err != nil {
			out = append(out, finding{Check: "upload_checkpoints", Severity: sevError, Detail: "cannot read upload checkpoints for " + agent, Remedy: "restore the join state checkpoints from a backup"})
		} else {
			count += len(snapshots)
		}
	}
	if len(out) == 0 {
		out = append(out, finding{Check: "upload_checkpoints", Severity: sevOK, Detail: fmt.Sprintf("%d upload checkpoints readable; hub parser stops are reported by the uploader", count)})
	}
	return out
}
