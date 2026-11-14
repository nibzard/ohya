// The doctor subcommand: read-only health checks for state the read-only sql
// MCP tool cannot reach — the hub database against this binary's schema and
// parser output versions, the FTS5 index, stopped sources in the local watch
// state, the device identity, the pairing credential, and the coordination
// plane a paired state directory depends on. Doctor never writes: it opens
// the database read-only, migrates nothing, repairs nothing, and re-derives
// nothing, and its findings carry paths, offsets, versions and remedies only
// — never transcript content or secrets.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/control"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

// Finding severities. Warnings do not fail the command; errors do.
const (
	sevOK    = "ok"
	sevWarn  = "warn"
	sevError = "error"
)

// errDoctorFailed marks a completed run whose findings include an error;
// cliMain maps it to exit 1 after the findings are already printed.
var errDoctorFailed = errors.New("doctor found one or more error findings")

// probeTimeout bounds the coordination-plane probe of a paired state
// directory: a hub that cannot answer within it is reported, not waited for.
const probeTimeout = 3 * time.Second

// doctorSpec is the doctor usage template.
var doctorSpec = commandSpec{
	name:      "ohya doctor",
	synopsis:  "run read-only health checks the sql tool cannot answer",
	usageLine: "ohya doctor [flags]",
	description: "Checks state outside the read-only sql tool's reach: the database schema against this build's migrations, stored parser output versions this build's rederive refuses, the FTS5 integrity of events_fts, stopped sources in the local watch state (ADR 0004), the device identity, and — when the state directory holds a pairing credential — whether the coordination plane answers within a short timeout. " +
		"Doctor opens the database read-only: it never migrates, repairs, or re-derives, and prints no transcript content or secrets.\n" +
		"Every finding carries a severity and a one-line remedy. Exit codes: 0 when no finding is an error, 1 otherwise; warnings do not fail. A joined device checks its own state with --state ~/.ohya/join and its hub's coordination URL through --control.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya doctor", "ohya doctor --state ~/.ohya/join --control http://hub:9417"},
}

// finding is one doctor result: the named check, its severity, what was
// observed, and the one-line remedy that clears it ("" when none is owed).
type finding struct {
	Check    string `json:"check"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
	Remedy   string `json:"remedy"`
}

// doctorJSONData is the doctor command's data payload (ADR 0005): every
// finding, in check order. Error findings ride in data, not in error — the
// findings are the report and the exit code is the verdict, like a degraded
// ingest pass.
type doctorJSONData struct {
	Findings []finding `json:"findings"`
}

// runDoctorCommand parses the doctor flags and runs the checks. With --json
// it prints one ADR 0005 envelope instead of the text findings.
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
	controlURL := c.fs.String("control", control.DefaultURL(),
		"coordination `URL` probed when the state directory holds a pairing credential")
	maxRecordBytes := c.fs.Int64("max-record-bytes", claudecode.DefaultMaxRecordBytes,
		"record size `bound` the stop check parses with; pass the bound ingestion last ran with (docs/adr/0004)")
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
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	findings, err := runDoctor(context.Background(), doctorOptions{
		dbPath:         *db,
		stateDir:       stateDir,
		controlURL:     *controlURL,
		maxRecordBytes: *maxRecordBytes,
	})
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, doctorSpec.name, err)
	}
	if *asJSON {
		if err := printJSON(stdout, doctorSpec.name, doctorJSONData{Findings: findings}); err != nil {
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

// doctorOptions configures one doctor run.
type doctorOptions struct {
	dbPath         string
	stateDir       string
	controlURL     string
	maxRecordBytes int64
}

// runDoctor runs every check and returns the findings, or a systemic failure
// no finding can represent (the embedded migrations being unreadable).
func runDoctor(ctx context.Context, o doctorOptions) ([]finding, error) {
	findings := make([]finding, 0)
	binSchema, err := store.SchemaVersion()
	if err != nil {
		return nil, err
	}
	var hub *store.DB
	switch _, statErr := os.Stat(o.dbPath); {
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
		findings = append(findings, checkFTSIndex(ctx, hub)...)
	}
	findings = append(findings, checkStoppedSources(ctx, hub, o)...)
	findings = append(findings, checkDeviceIdentity(o.stateDir))
	findings = append(findings, checkJoinState(ctx, o)...)
	return findings, nil
}

// checkSchema compares the database's applied schema version with this
// build's migrations. Doctor never migrates: a database behind the build
// warns with the command that migrates it; one ahead means an older binary.
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

// checkParserVersions compares every stored agent's highest parser output
// version with this build's parsers. A row stamped above the build's version
// is what store.ReDerive refuses: only a newer binary can re-derive it.
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

// checkFTSIndex runs the FTS5 integrity-check on events_fts, on a private
// snapshot, so the hub database stays read-only.
func checkFTSIndex(ctx context.Context, hub *store.DB) []finding {
	if err := hub.FTSIndexConsistent(ctx); err != nil {
		return []finding{{Check: "fts_index", Severity: sevError,
			Detail: fmt.Sprintf("events_fts failed the FTS5 integrity-check: %v", err),
			Remedy: "restore the database from a backup; ohya rederive rebuilds events_fts after a parser upgrade"}}
	}
	return []finding{{Check: "fts_index", Severity: sevOK,
		Detail: "events_fts passes the FTS5 integrity-check"}}
}

// checkStoppedSources re-decides, read-only, whether any watched source of
// the state directory is stopped at its acknowledged cursor: it parses the
// first pending record with the same bound ingestion uses. Only a stop at
// the cursor is reported — that is the checkpoint's state, not a guess about
// bytes a later pass reaches. The remedy is ADR 0004's decision.
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

// stoppedAt parses the bytes at one source's acknowledged cursor, bounded the
// way ingestion bounds a record, and reports a stop exactly when the parser
// stops at that offset. A record within the bound is fully visible in
// bound+1 bytes: at most the bound of JSON text plus one terminator.
func stoppedAt(ctx context.Context, hub *store.DB, agent string, s watch.SourceSnapshot, limit int64) (finding, bool) {
	info, err := os.Stat(s.Path)
	if err != nil || !info.Mode().IsRegular() {
		return finding{}, false // a missing or irregular file is not a stop; the watcher reports it
	}
	pending := info.Size() - s.CursorBytes
	if pending <= 0 {
		return finding{}, false // nothing pending: no stop can sit at the cursor
	}
	buf := make([]byte, min(pending, limit+1))
	f, err := os.Open(s.Path)
	if err != nil {
		return finding{}, false // read failures surface in ingestion's own diagnostics
	}
	defer f.Close()
	if _, err := f.ReadAt(buf, s.CursorBytes); err != nil {
		return finding{}, false
	}
	opts := events.Options{SourceScope: s.SourceID, MaxRecordBytes: limit}
	if hub != nil && s.CursorBytes > 0 {
		// Records past offset 0 may not name their session (Codex): the
		// committed first record does.
		if st, err := hub.ReadOnlySourceState(ctx, s.SourceID); err == nil {
			opts.FirstNative = st.FirstNativeID
		}
	}
	_, perr := events.Parse(agent, buf, s.CursorBytes, opts)
	var stop *events.StopError
	if !errors.As(perr, &stop) || stop.Offset != s.CursorBytes {
		return finding{}, false // pending bytes parse: the source is not stopped
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

// checkDeviceIdentity inspects the state directory's device identity without
// creating one — a missing identity is a warning with the command that
// creates it, an unusable one an error, because replacing it silently would
// fork the device identity.
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

// checkJoinState inspects the pairing credential. Without one, the state
// directory is local-only and there is nothing to probe. With one, the
// coordination plane must answer within probeTimeout — any HTTP response
// counts, because the plane may 404 on the bare URL.
func checkJoinState(ctx context.Context, o doctorOptions) []finding {
	cred, err := device.LoadCredential(o.stateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return []finding{{Check: "join_state", Severity: sevOK,
			Detail: fmt.Sprintf("no %s in %s; this state directory is not paired with a hub", device.CredentialFileName, o.stateDir)}}
	case err != nil:
		return []finding{{Check: "join_state", Severity: sevError,
			Detail: err.Error(),
			Remedy: "restore credential.json from a backup, or have the hub revoke this device identity and re-pair with a new one"}}
	}
	unreachable := func(detail, remedy string) []finding {
		return []finding{{Check: "join_state", Severity: sevWarn, Detail: detail, Remedy: remedy}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.controlURL, nil)
	if err != nil {
		return unreachable(
			fmt.Sprintf("paired as device %s, but the coordination URL %s is not valid: %v", cred.DeviceID, o.controlURL, err),
			"pass the coordination URL ohya join uses, through --control")
	}
	resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
	if err != nil {
		return unreachable(
			fmt.Sprintf("paired as device %s, but the coordination plane at %s did not answer: %v", cred.DeviceID, o.controlURL, err),
			"check that ohya serve runs on the hub and --control names its coordination URL")
	}
	resp.Body.Close()
	return []finding{{Check: "join_state", Severity: sevOK,
		Detail: fmt.Sprintf("paired as device %s; the coordination plane at %s answered", cred.DeviceID, o.controlURL)}}
}

// writeFindings prints one line per finding: severity, check, detail, and
// the remedy when one is owed.
func writeFindings(w io.Writer, findings []finding) {
	for _, f := range findings {
		line := fmt.Sprintf("%-5s %s: %s", f.Severity, f.Check, f.Detail)
		if f.Remedy != "" {
			line += "; remedy: " + f.Remedy
		}
		fmt.Fprintln(w, line)
	}
}

// hasError reports whether any finding carries the error severity.
func hasError(findings []finding) bool {
	for _, f := range findings {
		if f.Severity == sevError {
			return true
		}
	}
	return false
}
