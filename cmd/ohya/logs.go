package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

const (
	logsFormatVersion      = 1
	logsDefaultSince       = 10 * time.Minute
	logsDefaultLimit       = 100
	logsMaxLimit           = 1000
	logsMaxPayloadBytes    = 1 << 20
	logsReadBoundPerFile   = observe.LogMaxFileBytes + observe.LogLineMaxBytes
	logsFollowPollInterval = 200 * time.Millisecond
)

var logsSpec = commandSpec{
	name:      "ohya logs",
	synopsis:  "read the structured application logs ohya processes write",
	usageLine: "ohya logs [flags]",
	description: "Reads the bounded events.jsonl files each long-running ohya process writes under the state directory's observe/ tree. " +
		"Entries carry curated event codes and numeric counts only; they never contain transcript text, SQL, or paths. " +
		"--level filters to that severity and above; --limit keeps the newest matching entries (default 100, maximum 1000). " +
		"A finite report stops at 1 MiB of entries and says so; --follow streams one instance, replays its lookback window once, and follows rotation. " +
		"Logs are local only; this command never contacts the hub. " +
		"Exit codes: 0 for a valid result, 3 for a partial or degraded result, 1 for failure, 2 for usage.",
	examples: []string{"ohya logs --since 10m --json", "ohya logs --follow --instance abc12345"},
}

type logsInstance struct {
	InstanceID    string   `json:"instance_id"`
	Roles         []string `json:"roles,omitempty"`
	Files         []string `json:"files"`
	Entries       int      `json:"entries"`
	SkippedLines  int      `json:"skipped_lines"`
	ReadTruncated bool     `json:"read_truncated"`
}

type logsData struct {
	FormatVersion    int                `json:"format_version"`
	SinceMS          int64              `json:"since_ms"`
	SinceText        string             `json:"since_text"`
	Limit            int                `json:"limit"`
	GeneratedWallMS  int64              `json:"generated_wall_ms"`
	GeneratedUTC     string             `json:"generated_utc"`
	Instances        []logsInstance     `json:"instances"`
	Entries          []observe.LogEntry `json:"entries"`
	Truncated        bool               `json:"truncated"`
	TruncationReason string             `json:"truncation_reason,omitempty"`
	Findings         []statusFinding    `json:"findings"`
	Coverage         statusCoverage     `json:"coverage"`
}

type logsOptions struct {
	stateDir   string
	role       string
	instanceID string
	since      time.Duration
	limit      int
	level      string
	component  string
	follow     bool
	asJSON     bool
	nowMS      func() int64
}

func runLogsCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(logsSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` used to resolve the default state directory")
	state := c.fs.String("state", "",
		"state `directory` whose observe/ tree holds application logs; defaults to the database directory")
	role := c.fs.String("role", "",
		"restrict the report to instances carrying this `role` (serve, join, ingest, mcp)")
	instance := c.fs.String("instance", "",
		"restrict the report to one instance `id`; with --follow, disambiguate when several instances publish")
	since := c.fs.Duration("since", logsDefaultSince,
		"show entries no older than this `duration` (example: 10m, 1h)")
	limit := c.fs.Int("limit", logsDefaultLimit,
		"show at most this many `entries`; the newest win (maximum 1000)")
	level := c.fs.String("level", "",
		"show entries at this `level` or above: debug, info, warn, error")
	component := c.fs.String("component", "",
		"show entries from this `component` (example: ingest, uploader, mcp, observe)")
	follow := c.fs.Bool("follow", false,
		"keep streaming new entries from one instance after replaying its lookback window; follows rotation")
	asJSON := c.fs.Bool("json", false,
		"print JSON instead of the text report; with --follow emit one envelope per line (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if *since <= 0 {
		return c.usageErrorf("--since must be positive")
	}
	if *limit <= 0 || *limit > logsMaxLimit {
		return c.usageErrorf("--limit must be between 1 and %d", logsMaxLimit)
	}
	if *level != "" && !validLogLevel(*level) {
		return c.usageErrorf("--level must be one of debug, info, warn, error")
	}
	if *role != "" {
		if _, ok := observe.ParseRole(*role); !ok {
			return c.usageErrorf("--role must be one of serve, join, ingest, mcp")
		}
	}
	stateDir, _, err := resolveObserveStateDir(*state, *db)
	if err != nil {
		return jsonFailurePrint(*asJSON && !*follow, stdout, logsSpec.name, err)
	}
	o := logsOptions{
		stateDir:   stateDir,
		role:       *role,
		instanceID: *instance,
		since:      *since,
		limit:      *limit,
		level:      *level,
		component:  *component,
		follow:     *follow,
		asJSON:     *asJSON,
		nowMS:      func() int64 { return time.Now().UnixMilli() },
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = runLogs(ctx, o, stdout, stderr)
	if err != nil && !errors.Is(err, errDegraded) {
		return jsonFailurePrint(*asJSON && !*follow, stdout, logsSpec.name, err)
	}
	return err
}

func validLogLevel(level string) bool {
	switch observe.Level(level) {
	case observe.LevelDebug, observe.LevelInfo, observe.LevelWarn, observe.LevelError:
		return true
	}
	return false
}

func levelRank(level observe.Level) int {
	switch level {
	case observe.LevelDebug:
		return 0
	case observe.LevelInfo:
		return 1
	case observe.LevelWarn:
		return 2
	default:
		return 3
	}
}

func (o logsOptions) matches(entry observe.LogEntry, sinceMS int64) bool {
	if sinceMS > 0 && entry.TimeMS < sinceMS {
		return false
	}
	if o.level != "" && levelRank(entry.Level) < levelRank(observe.Level(o.level)) {
		return false
	}
	if o.component != "" && entry.Component != o.component {
		return false
	}
	return true
}

func runLogs(ctx context.Context, o logsOptions, stdout, stderr io.Writer) error {
	var views []observe.InstanceView
	if o.instanceID != "" {
		view, err := observe.LoadInstanceView(filepath.Join(o.stateDir, "observe"), o.instanceID)
		if err != nil {
			return err
		}
		views = []observe.InstanceView{view}
	} else {
		var truncated bool
		var err error
		views, truncated, err = observe.DiscoverInstances(filepath.Join(o.stateDir, "observe"))
		if err != nil {
			return err
		}
		if truncated {
			return fmt.Errorf("more than %d instances publish under %s; pass --instance to select one", observe.MaxLiveInstances, o.stateDir)
		}
	}
	selected := []observe.InstanceView{}
	for _, view := range views {
		if o.role != "" && !view.HasRole(o.role) {
			continue
		}
		selected = append(selected, view)
	}
	if o.follow {
		if len(selected) != 1 {
			return fmt.Errorf("--follow needs exactly one instance, matched %d; pass --instance or --role to select one", len(selected))
		}
		return followLogs(ctx, selected[0], o, stdout)
	}
	nowMS := o.nowMS()
	data := logsData{
		FormatVersion:   logsFormatVersion,
		SinceMS:         o.since.Milliseconds(),
		SinceText:       o.since.String(),
		Limit:           o.limit,
		GeneratedWallMS: nowMS,
		GeneratedUTC:    time.UnixMilli(nowMS).UTC().Format(time.RFC3339),
		Instances:       []logsInstance{},
		Entries:         []observe.LogEntry{},
		Findings:        []statusFinding{},
	}
	sinceMS := nowMS - o.since.Milliseconds()
	var all []observe.LogEntry
	for _, view := range selected {
		report := logsInstance{InstanceID: view.InstanceID, Files: []string{}}
		if view.Snapshot != nil {
			report.Roles = view.Snapshot.Instance.Roles
		}
		entries, files, skipped, readTruncated := readInstanceLogs(view.Dir)
		report.Files = files
		report.SkippedLines = skipped
		report.ReadTruncated = readTruncated
		for _, entry := range entries {
			if o.matches(entry, sinceMS) {
				report.Entries++
				all = append(all, entry)
			}
		}
		data.Instances = append(data.Instances, report)
		if skipped > 0 {
			data.Findings = append(data.Findings, statusFinding{
				Code:     "log_lines_skipped",
				Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s: %d log lines were oversize or unreadable and are not shown", shortID(view.InstanceID), skipped),
				Hint:     "a newer or broken build wrote these lines; only valid bounded entries are reported",
			})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TimeMS < all[j].TimeMS })
	if len(all) > o.limit {
		all = all[len(all)-o.limit:]
		data.Truncated = true
		data.TruncationReason = fmt.Sprintf("the limit of %d entries kept the newest matches", o.limit)
	}
	payload := 0
	start := len(all)
	for start > 0 {
		encoded, err := json.Marshal(all[start-1])
		if err != nil {
			break
		}
		if payload+len(encoded) > logsMaxPayloadBytes && len(all)-start > 0 {
			data.Truncated = true
			data.TruncationReason = fmt.Sprintf("the finite payload bound of %d bytes dropped the oldest %d matching entries", logsMaxPayloadBytes, start)
			break
		}
		payload += len(encoded)
		start--
	}
	data.Entries = make([]observe.LogEntry, 0, len(all)-start)
	data.Entries = append(data.Entries, all[start:]...)
	if len(selected) == 0 && o.instanceID == "" {
		data.Coverage.Missing = append(data.Coverage.Missing, "instances")
		data.Findings = append(data.Findings, statusFinding{
			Code:     "no_instances",
			Severity: sevWarn,
			Evidence: fmt.Sprintf("no instance publishes logs under %s", filepath.Join(o.stateDir, "observe")),
			Hint:     "no long-running ohya process may be running in this state directory, or one runs with --observe off",
		})
	} else if !logsDegraded(data) {
		data.Findings = append(data.Findings, statusFinding{
			Code:     "logs_complete",
			Severity: sevOK,
			Evidence: "every selected instance's bounded log files were read within their caps",
		})
	}
	if o.asJSON {
		if err := printJSON(stdout, logsSpec.name, data); err != nil {
			return err
		}
	} else {
		writeLogsText(stdout, data)
	}
	if logsDegraded(data) {
		return errDegraded
	}
	return nil
}

func logsDegraded(data logsData) bool {
	for _, f := range data.Findings {
		if f.Severity == sevWarn || f.Severity == sevError {
			return true
		}
	}
	return false
}

func readInstanceLogs(dir string) (entries []observe.LogEntry, files []string, skipped int, readTruncated bool) {
	out := []observe.LogEntry{}
	names := []string{}
	rotated := make([]string, 0, observe.LogRotatedFiles)
	for i := observe.LogRotatedFiles; i >= 1; i-- {
		rotated = append(rotated, fmt.Sprintf("events.jsonl.%d", i))
	}
	for _, name := range append(rotated, "events.jsonl") {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		names = append(names, name)
		fileEntries, fileSkipped, truncated := readLogFile(path)
		skipped += fileSkipped
		readTruncated = readTruncated || truncated
		out = append(out, fileEntries...)
	}
	return out, names, skipped, readTruncated
}

func readLogFile(path string) (entries []observe.LogEntry, skipped int, truncated bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, logsReadBoundPerFile))
	for {
		line, err := readLogLine(reader)
		if errors.Is(err, errOversizeLogLine) {
			skipped++
			continue
		}
		if len(line) > 0 {
			var entry observe.LogEntry
			if jerr := json.Unmarshal(line, &entry); jerr != nil || entry.Validate() != nil {
				skipped++
			} else {
				entries = append(entries, entry)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				truncated = true
			}
			break
		}
	}
	if info, serr := os.Stat(path); serr == nil && info.Size() > logsReadBoundPerFile {
		truncated = true
	}
	return entries, skipped, truncated
}

var errOversizeLogLine = errors.New("log line exceeds the bounded line size")

func readLogLine(reader *bufio.Reader) (line []byte, err error) {
	var buf []byte
	for {
		chunk, rerr := reader.ReadSlice('\n')
		if len(buf)+len(chunk) > observe.LogLineMaxBytes {
			for rerr == bufio.ErrBufferFull {
				_, rerr = reader.ReadBytes('\n')
			}
			return nil, errOversizeLogLine
		}
		buf = append(buf, chunk...)
		if rerr == nil {
			return buf[:len(buf)-1], nil
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(rerr, io.EOF) {
			if len(buf) > 0 {
				return buf, io.EOF
			}
			return nil, io.EOF
		}
		return buf, rerr
	}
}

func entryText(entry observe.LogEntry) string {
	return entryTextStyled(cliStyle{}, entry)
}

func entryTextStyled(s cliStyle, entry observe.LogEntry) string {
	stamp := time.UnixMilli(entry.TimeMS).UTC().Format(time.RFC3339)
	parts := []string{
		s.dim(stamp),
		s.logLevel(string(entry.Level)),
		s.cyan(entry.Component),
		s.bold(entry.EventCode),
	}
	if entry.Outcome != "" {
		parts = append(parts, entry.Outcome)
	}
	if len(entry.Counts) > 0 {
		counts := make([]string, 0, len(entry.Counts))
		for _, name := range sortedKeys(entry.Counts) {
			counts = append(counts, fmt.Sprintf("%s=%d", name, entry.Counts[name]))
		}
		parts = append(parts, s.dim(strings.Join(counts, " ")))
	}
	return strings.Join(parts, " ")
}

func writeLogsText(w io.Writer, data logsData) {
	s := styleFor(w)
	sep := " " + s.bullet() + " "
	fmt.Fprintf(w, "%s  %s\n", s.bold("ohya logs"), s.dim(fmt.Sprintf("since %s%slimit %d%s%s",
		data.SinceText, sep, data.Limit, sep, data.GeneratedUTC)))
	var activeSummaries []string
	silent := 0
	for _, instance := range data.Instances {
		if instance.Entries == 0 && instance.SkippedLines == 0 && !instance.ReadTruncated {
			silent++
			continue
		}
		role := strings.Join(instance.Roles, ",")
		if role == "" {
			role = "unknown"
		}
		summary := fmt.Sprintf("%s %s (%d)", s.cyan(role), shortID(instance.InstanceID), instance.Entries)
		if instance.SkippedLines > 0 {
			summary += s.yellow(fmt.Sprintf(" [%d skipped]", instance.SkippedLines))
		}
		if instance.ReadTruncated {
			summary += s.yellow(" [truncated]")
		}
		activeSummaries = append(activeSummaries, summary)
	}
	if len(activeSummaries) > 0 {
		line := strings.Join(activeSummaries, sep)
		if silent > 0 {
			line += " " + s.dim(fmt.Sprintf("(+%d silent)", silent))
		}
		fmt.Fprintf(w, "%s  %s\n", s.dim("Sources"), s.fit(line, 9))
	} else if silent > 0 {
		fmt.Fprintf(w, "%s  %s\n", s.dim("Sources"), s.dim(fmt.Sprintf("%d checked, 0 matching entries", silent)))
	}
	if len(data.Entries) > 0 {
		fmt.Fprintln(w)
		for _, entry := range data.Entries {
			fmt.Fprintln(w, entryTextStyled(s, entry))
		}
	}
	if data.Truncated {
		fmt.Fprintf(w, "\n%s\n", s.yellow("truncated: "+data.TruncationReason))
	}
	writeStatusFindingsText(w, s, data.Findings)
}

type logsFollowData struct {
	FormatVersion int              `json:"format_version"`
	Entry         observe.LogEntry `json:"entry"`
}

func followLogs(ctx context.Context, view observe.InstanceView, o logsOptions, stdout io.Writer) error {
	path := filepath.Join(view.Dir, "events.jsonl")
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("instance %s has no readable events.jsonl yet", shortID(view.InstanceID))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	tail := &logTailer{path: path, file: file, info: info, reader: bufio.NewReader(file)}
	s := styleFor(stdout)
	emit := func(entry observe.LogEntry) {
		if !o.matches(entry, 0) {
			return
		}
		if o.asJSON {
			_ = printJSON(stdout, logsSpec.name, logsFollowData{FormatVersion: logsFormatVersion, Entry: entry})
			return
		}
		fmt.Fprintln(stdout, entryTextStyled(s, entry))
	}
	sinceMS := o.nowMS() - o.since.Milliseconds()
	backlog, _, _ := readLogFile(path)
	var replay []observe.LogEntry
	for _, entry := range backlog {
		if o.matches(entry, sinceMS) {
			replay = append(replay, entry)
		}
	}
	if len(replay) > o.limit {
		replay = replay[len(replay)-o.limit:]
	}
	for _, entry := range replay {
		emit(entry)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	tail.reader = bufio.NewReader(file)
	for {
		entries, err := tail.poll()
		if err != nil {
			return fmt.Errorf("follow instance %s: %w", shortID(view.InstanceID), err)
		}
		for _, entry := range entries {
			emit(entry)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logsFollowPollInterval):
		}
	}
}

type logTailer struct {
	path   string
	file   *os.File
	info   os.FileInfo
	reader *bufio.Reader
}

func (t *logTailer) poll() ([]observe.LogEntry, error) {
	if current, err := os.Stat(t.path); err == nil && !os.SameFile(current, t.info) {
		entries, _ := t.read()
		if err := t.reopen(); err != nil {
			return entries, nil
		}
		fresh, _ := t.read()
		return append(entries, fresh...), nil
	}
	return t.read()
}

func (t *logTailer) reopen() error {
	file, err := os.Open(t.path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	_ = t.file.Close()
	t.file = file
	t.info = info
	t.reader = bufio.NewReader(file)
	return nil
}

func (t *logTailer) read() ([]observe.LogEntry, error) {
	var entries []observe.LogEntry
	for {
		line, err := readLogLine(t.reader)
		if errors.Is(err, errOversizeLogLine) {
			continue
		}
		if len(line) > 0 {
			var entry observe.LogEntry
			if jerr := json.Unmarshal(line, &entry); jerr == nil && entry.Validate() == nil {
				entries = append(entries, entry)
			}
		}
		if err != nil {
			return entries, nil
		}
	}
}
