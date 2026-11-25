package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/antigravity"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
	"github.com/nibzard/ohya/internal/workspace"
)

const maxBatchBytes int64 = 8 << 20

type agentRoot struct {
	agent string
	flag  string
	dirs  []agentDir
}

type agentDir struct {
	dir      string
	optional bool
}

type ingestOptions struct {
	dbPath         string
	stateDir       string
	roots          []agentRoot
	excludes       excludeGlobs
	interval       time.Duration
	allowEmpty     bool
	once           bool
	maxRecordBytes int64
	jsonOut        bool
	stdout         io.Writer
	stderr         io.Writer
	observer       observe.Observer
	events         observe.EventSink
}

var errDegraded = errors.New("ingest completed with a failed or stopped source")

func runIngest(ctx context.Context, o ingestOptions) error {
	if o.observer == nil {
		o.observer = observe.Disabled
	}
	if len(o.roots) == 0 {
		return errors.New("no agent session directory to watch")
	}
	present := 0
	for _, root := range o.roots {
		if !events.Supported(root.agent) {
			return fmt.Errorf("agent %q has no parser", root.agent)
		}
		rootPresent := false
		for _, d := range root.dirs {
			_, err := os.Stat(d.dir)
			switch {
			case err == nil:
				rootPresent = true
			case !errors.Is(err, os.ErrNotExist):
				return fmt.Errorf("stat %s: %w", d.dir, err)
			case !d.optional && !o.allowEmpty:
				return classify(fmt.Errorf("%s session directory %s does not exist; pass --%s", root.agent, d.dir, root.flag),
					"missing_agent_root", "pass the named --<agent>-root flag, or create the directory")
			}
		}
		if rootPresent {
			present++
		}
	}
	if present == 0 && !o.allowEmpty {
		var flags []string
		for _, root := range o.roots {
			flags = append(flags, "--"+root.flag)
		}
		return classify(fmt.Errorf("no agent session directory exists; pass %s", strings.Join(flags, " or ")),
			"missing_agent_root", "pass or create at least one agent session directory")
	}
	if err := os.MkdirAll(filepath.Dir(o.dbPath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(o.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if len(o.excludes) > 0 {
		fmt.Fprintf(o.stderr, "ohya: excluding session files matching: %s\n", strings.Join(o.excludes, ", "))
	}
	openOpts := []store.OpenOption{store.WithObserver(o.observer)}
	if !o.once {
		openOpts = append(openOpts, store.WaitForLock(reportLockWait(o.stderr)))
	}
	hub, err := store.Open(ctx, o.dbPath, openOpts...)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return classify(err, "database_open_failed", "check the --db path and its permissions; another ohya process may hold the database")
	}
	defer hub.Close()

	deviceID, err := device.LoadOrCreate(o.stateDir)
	if err != nil {
		return err
	}
	runners := make([]*ingestor, len(o.roots))
	for i, root := range o.roots {
		w, err := watch.New(o.stateDir, root.agent, watch.Options{Observer: o.observer})
		if err != nil {
			return err
		}
		runners[i] = newIngestor(hub, w, root.agent, deviceID, o.maxRecordBytes, o.excludes, o.stderr, o.observer, o.events)
		if root.agent == antigravity.AgentName {
			runners[i].sourceWorkspace = antigravitySourceWorkspace(root.dirs[0].dir, o.stderr)
		}
	}
	var excludedByAgent map[string]int
	if len(o.excludes) > 0 {
		excludedByAgent = make(map[string]int, len(o.roots))
		for _, root := range o.roots {
			excludedByAgent[root.agent] = 0
		}
	}

	for pass := 1; ; pass++ {
		var stats passStats
		passStart := time.Now()
		for i, r := range runners {
			s, err := r.poll(ctx, o.roots[i].dirs, o.allowEmpty)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			stats.sources += s.sources
			stats.inserted += s.inserted
			stats.failed += s.failed
			stats.stopped += s.stopped
			stats.excluded += s.excluded
			stats.stops = append(stats.stops, s.stops...)
			if len(o.excludes) > 0 {
				excludedByAgent[o.roots[i].agent] = s.excluded
			}
		}
		o.observer.Add("ingest_passes", 1)
		o.observer.Observe("ingest_pass_duration", time.Since(passStart))
		switch {
		case o.jsonOut:
			stops := stats.stops
			if stops == nil {
				stops = []stopReport{}
			}
			excluded := excludedByAgent
			if excluded == nil {
				excluded = map[string]int{}
			}
			if err := printJSON(o.stdout, ingestSpec.name, ingestJSONData{
				Events:   stats.inserted,
				Sources:  stats.sources,
				Excluded: excluded,
				Failed:   stats.failed,
				Stopped:  stats.stopped,
				Stops:    stops,
			}); err != nil {
				return err
			}
		case o.once || pass == 1 || stats.inserted > 0:
			summary := fmt.Sprintf("ohya: imported %d events from %d sources", stats.inserted, stats.sources)
			if stats.failed > 0 || stats.stopped > 0 {
				summary += fmt.Sprintf("; %d failed, %d stopped", stats.failed, stats.stopped)
			}
			if len(o.excludes) > 0 {
				summary += fmt.Sprintf("; %d files excluded by --exclude (%s)", stats.excluded, excludedSummary(excludedByAgent))
			}
			fmt.Fprintln(o.stdout, summary)
		}
		if o.once {
			if stats.failed > 0 || stats.stopped > 0 {
				return errDegraded
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(o.interval):
		}
	}
}

type ingestor struct {
	hub             *store.DB
	watcher         *watch.Watcher
	agent           string
	deviceID        string
	maxRecordBytes  int64
	excludes        excludeGlobs
	stderr          io.Writer
	observer        observe.Observer
	events          observe.EventSink
	reported        map[string]int64
	missingReported map[string]bool
	present         map[string]bool
	excludedPaths   map[string]bool
	noProgress      map[string]noProgressMark
	tails           map[string]int64
	registered      map[string]string
	purged          map[string]bool
	sourceWorkspace func(sourcePath string) (string, bool)
	workspaceTried  map[string]bool
	workspaceFailed map[string]bool
	workspacesDue   bool
}

type noProgressMark struct {
	end            int64
	cursor         int64
	modTime        time.Time
	stopped        bool
	stopOffset     int64
	recordTooLarge bool
}

func newIngestor(hub *store.DB, w *watch.Watcher, agent, deviceID string, maxRecordBytes int64, excludes excludeGlobs, stderr io.Writer, observer observe.Observer, events observe.EventSink) *ingestor {
	if observer == nil {
		observer = observe.Disabled
	}
	return &ingestor{
		hub: hub, watcher: w, agent: agent, deviceID: deviceID,
		maxRecordBytes: maxRecordBytes, excludes: excludes, stderr: stderr, observer: observer, events: events, reported: map[string]int64{},
		missingReported: map[string]bool{}, present: map[string]bool{}, noProgress: map[string]noProgressMark{}, tails: map[string]int64{},
		registered: map[string]string{}, purged: map[string]bool{}, workspacesDue: true,
		workspaceTried: map[string]bool{}, workspaceFailed: map[string]bool{},
	}
}

func (r *ingestor) event(level observe.Level, code observe.EventCode, outcome string, counts map[string]int64) {
	if r.events == nil {
		return
	}
	r.events.Event("ingest", level, code, outcome, counts)
}

type passStats struct {
	sources  int
	inserted int
	failed   int
	stopped  int
	excluded int
	stops    []stopReport
}

type stopReport struct {
	SourceID string `json:"source_id"`
	Path     string `json:"path"`
	Offset   int64  `json:"offset"`
	Remedy   string `json:"remedy"`
}

type ingestJSONData struct {
	Events   int            `json:"events"`
	Sources  int            `json:"sources"`
	Excluded map[string]int `json:"excluded"`
	Failed   int            `json:"failed"`
	Stopped  int            `json:"stopped"`
	Stops    []stopReport   `json:"stops"`
}

func excludedSummary(byAgent map[string]int) string {
	if len(byAgent) == 0 {
		return "no matches"
	}
	agents := make([]string, 0, len(byAgent))
	for agent := range byAgent {
		agents = append(agents, agent)
	}
	slices.Sort(agents)
	parts := make([]string, 0, len(agents))
	for _, agent := range agents {
		parts = append(parts, fmt.Sprintf("%s: %d", agent, byAgent[agent]))
	}
	return strings.Join(parts, ", ")
}

func (r *ingestor) poll(ctx context.Context, dirs []agentDir, allowEmpty bool) (passStats, error) {
	discoveryStart := time.Now()
	candidates, excluded, err := discoverAgentSessionFiles(r.agent, dirs, r.excludes, allowEmpty)
	if err != nil {
		r.observer.Add("discovery_errors", 1, "read")
		return passStats{}, err
	}
	r.excludedPaths = excludedPathSet(excluded)
	r.observer.Add("discovery_scans", 1)
	r.observer.Add("discovery_candidates", uint64(len(candidates)))
	if len(r.excludes) > 0 {
		r.observer.Set("discovery_excluded", float64(len(excluded)), r.agent)
	}
	r.observer.Observe("discovery_duration", time.Since(discoveryStart))
	outcomes, err := r.watcher.Poll(candidates)
	if err != nil {
		return passStats{}, err
	}
	var stats passStats
	stats.excluded = len(excluded)
	pendingComplete := int64(0)
	grew := map[string]bool{}
	var missingAtStartup []string
	for _, o := range outcomes {
		if ctx.Err() != nil {
			break
		}
		if o.Missing {
			if r.reportMissing(o) {
				missingAtStartup = append(missingAtStartup, o.Path)
			}
			delete(r.tails, o.SourceID)
			continue
		}
		if r.missingReported[o.SourceID] {
			r.observer.Add("watch_source_events", 1, "returned")
			r.event(observe.LevelInfo, observe.EventSourceReturned, "", nil)
		}
		delete(r.missingReported, o.SourceID)
		r.present[o.SourceID] = true
		stats.sources++
		if o.Fresh {
			if o.ReplacedSourceID != "" {
				delete(r.noProgress, o.ReplacedSourceID)
				delete(r.tails, o.ReplacedSourceID)
				delete(r.registered, o.ReplacedSourceID)
				delete(r.purged, o.ReplacedSourceID)
			}
			if o.Reason != "new file" {
				fmt.Fprintf(r.stderr, "ohya: %s: new source %s re-imports from byte zero: %s\n",
					o.Path, o.SourceID, o.Reason)
			}
		}
		if r.purged[o.SourceID] {
			continue
		}
		if r.registered[o.SourceID] != o.Path {
			if err := r.hub.RegisterSource(ctx, o.SourceID, r.deviceID, r.agent, o.Path); err != nil {
				if ctx.Err() != nil {
					break
				}
				r.reportSource(o.Path, err)
				if errors.Is(err, store.ErrSourcePurged) {
					r.purged[o.SourceID] = true
					delete(r.registered, o.SourceID)
					delete(r.tails, o.SourceID)
					continue
				}
				r.observer.Add("ingest_failures", 1, "commit")
				r.event(observe.LevelWarn, observe.EventCommitFailed, "register", nil)
				stats.failed++
				pendingComplete += o.End - o.Cursor
				continue
			}
			r.registered[o.SourceID] = o.Path
		}
		if o.End <= o.Cursor {
			continue
		}
		if mark, cached := r.noProgress[o.SourceID]; cached && mark.end == o.End && mark.cursor == o.Cursor && mark.modTime.Equal(o.ModTime) {
			if mark.stopped {
				stats.stopped++
				stats.stops = append(stats.stops, stopReport{
					SourceID: o.SourceID,
					Path:     o.Path,
					Offset:   mark.stopOffset,
					Remedy:   stopRemedy(mark.recordTooLarge),
				})
			}
			continue
		}
		inserted, committed, stop, err := r.ingestSource(ctx, o)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.reportSource(o.Path, err)
			if errors.Is(err, store.ErrSourcePurged) {
				r.purged[o.SourceID] = true
				delete(r.registered, o.SourceID)
				delete(r.tails, o.SourceID)
				continue
			}
			stats.failed++
			pendingComplete += o.End - o.Cursor
			if errors.Is(err, store.ErrSourceNotRegistered) {
				delete(r.registered, o.SourceID)
			}
			continue
		}
		mark := noProgressMark{end: o.End, cursor: committed, modTime: o.ModTime}
		if stop != nil {
			stats.stopped++
			stats.stops = append(stats.stops, stopReport{
				SourceID: o.SourceID,
				Path:     o.Path,
				Offset:   stop.Offset,
				Remedy:   stopRemedy(stop.RecordTooLarge),
			})
			mark.stopped = true
			mark.stopOffset = stop.Offset
			mark.recordTooLarge = stop.RecordTooLarge
			delete(r.tails, o.SourceID)
		} else {
			r.tails[o.SourceID] = o.End - committed
		}
		stats.inserted += inserted
		if inserted > 0 {
			grew[o.SourceID] = true
		}
		if err := r.watcher.Acknowledge(o.Path, o.SourceID, committed); err != nil {
			r.reportSource(o.Path, err)
			r.observer.Add("ingest_failures", 1, "checkpoint")
			r.event(observe.LevelWarn, observe.EventCheckpointFailed, "acknowledge", nil)
			stats.failed++
			continue
		}
		if committed == o.Cursor {
			r.noProgress[o.SourceID] = mark
		} else {
			delete(r.noProgress, o.SourceID)
		}
	}
	r.reportMissingAtStartup(missingAtStartup)
	var tails, stopped int64
	for _, v := range r.tails {
		tails += v
	}
	for _, mark := range r.noProgress {
		if mark.stopped {
			stopped++
		}
	}
	r.observer.Set("watch_partial_tail_bytes", float64(tails))
	r.observer.Set("watch_pending_complete_bytes", float64(pendingComplete))
	r.observer.Set("watch_sources", float64(stopped), "stopped")
	checkpointStart := time.Now()
	if err := r.watcher.Flush(); err != nil {
		r.observer.Observe("ingest_checkpoint_duration", time.Since(checkpointStart))
		r.observer.Add("ingest_failures", 1, "checkpoint")
		r.event(observe.LevelWarn, observe.EventCheckpointFailed, "flush", nil)
		return stats, err
	}
	r.observer.Observe("ingest_checkpoint_duration", time.Since(checkpointStart))
	if r.sourceWorkspace != nil && ctx.Err() == nil && r.fillSourceWorkspaces(ctx, grew) {
		r.workspacesDue = true
	}
	if ctx.Err() == nil && (r.workspacesDue || stats.inserted > 0) {
		r.observeWorkspaces(ctx)
	}
	return stats, nil
}

func (r *ingestor) fillSourceWorkspaces(ctx context.Context, grew map[string]bool) bool {
	sources, err := r.hub.SourcesWithoutWorkspace(ctx, r.deviceID, r.agent)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(r.stderr, "ohya: %s: sources without a working directory, retrying next pass: %v\n", r.agent, err)
		}
		return false
	}
	set := false
	for _, s := range sources {
		if r.workspaceTried[s.ID] && !grew[s.ID] {
			continue
		}
		r.workspaceTried[s.ID] = true
		path, ok := r.sourceWorkspace(s.Path)
		if !ok {
			continue
		}
		err := r.hub.SetSourceWorkspace(ctx, r.deviceID, s.ID, path)
		switch {
		case err == nil:
			set = true
		case r.workspaceFailed[s.ID]:
		default:
			r.workspaceFailed[s.ID] = true
			r.reportSource(s.Path, fmt.Errorf("set the working directory %s: %w", path, err))
		}
	}
	return set
}

func (r *ingestor) observeWorkspaces(ctx context.Context) {
	res, err := workspace.Sync(ctx, func(ctx context.Context, observed []workspace.Observation) ([]string, error) {
		if _, err := r.hub.ObserveWorkspaces(ctx, r.deviceID, observed); err != nil {
			return nil, err
		}
		return r.hub.PendingWorkspaces(ctx, r.deviceID, r.agent, hubsync.MaxWorkspaces)
	}, workspace.Resolve)
	if n := len(res.Failures); n > 0 {
		fmt.Fprintf(r.stderr, "ohya: %d %s session directories could not be read and stay pending: %v\n", n, r.agent, res.Failures[0])
	}
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(r.stderr, "ohya: observe %s workspaces, retrying next pass: %v\n", r.agent, err)
		}
		return
	}
	r.workspacesDue = false
}

func (r *ingestor) ingestSource(ctx context.Context, o watch.Outcome) (int, int64, *events.StopError, error) {
	inserted := 0
	readEnd := min(o.End, o.Cursor+maxBatchBytes)
	for o.Cursor < o.End {
		chunk := o
		chunk.End = readEnd
		data, err := r.watcher.ReadPending(chunk)
		if err != nil {
			r.observer.Add("ingest_failures", 1, "read")
			return inserted, o.Cursor, nil, err
		}
		opts := events.Options{SourceScope: o.SourceID, SourcePath: o.Path, MaxRecordBytes: r.maxRecordBytes}
		if chunk.Cursor > 0 {
			st, err := r.hub.SourceState(ctx, o.SourceID)
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, store.ErrSourcePurged) {
					r.observer.Add("ingest_failures", 1, "read")
				}
				return inserted, o.Cursor, nil, err
			}
			opts.FirstNative = st.FirstNativeID
		}
		parseStart := time.Now()
		res, perr := events.Parse(r.agent, data, chunk.Cursor, opts)
		parseElapsed := time.Since(parseStart)
		r.observer.Add("parse_bytes", uint64(len(data)))
		r.observer.Observe("ingest_parse_duration", parseElapsed)
		r.observer.Observe("parse_duration", parseElapsed, r.agent)
		var stop *events.StopError
		stopped := errors.As(perr, &stop)
		attempted := uint64(len(res.Events))
		if stopped {
			attempted++
		}
		r.observer.Add("parse_records", attempted, "attempted")
		r.observer.Add("parse_records", uint64(len(res.Events)), "normalized")
		if stopped {
			r.observer.Add("parse_records", 1, "stopped")
			stopCode := "invalid"
			if stop.RecordTooLarge {
				r.observer.Add("parse_stops", 1, "oversize")
				stopCode = "oversize"
			} else {
				r.observer.Add("parse_stops", 1, "invalid")
			}
			r.event(observe.LevelWarn, observe.EventSourceStopped, stopCode, nil)
		}
		if len(res.Events) > 0 {
			r.observer.Add("ingest_batches", 1, "attempted")
			result, err := r.hub.Ingest(ctx, store.Batch{
				SourceID: o.SourceID,
				DeviceID: r.deviceID,
				Events:   res.Events,
			})
			if err != nil {
				switch {
				case ctx.Err() != nil:
				case errors.Is(err, store.ErrBatchRejected), errors.Is(err, store.ErrSourcePurged):
					r.observer.Add("ingest_batches", 1, "rejected")
				default:
					r.observer.Add("ingest_batches", 1, "rolled_back")
					r.event(observe.LevelWarn, observe.EventCommitFailed, "ingest", nil)
				}
				return inserted, o.Cursor, nil, err
			}
			r.observer.Add("ingest_batches", 1, "committed")
			r.observer.Add("ingest_events", uint64(result.Inserted), "inserted")
			r.observer.Add("ingest_events", uint64(result.Duplicates), "duplicate")
			r.observer.Add("ingest_bytes", uint64(result.Cursor-o.Cursor))
			inserted += result.Inserted
			o.Cursor = result.Cursor
			readEnd = min(o.End, o.Cursor+maxBatchBytes)
		}
		if stopped {
			r.reportStop(o, stop)
			return inserted, o.Cursor, stop, nil
		}
		if perr != nil {
			return inserted, o.Cursor, nil, perr
		}
		if len(res.Events) == 0 {
			if chunk.End >= o.End {
				return inserted, o.Cursor, nil, nil
			}
			readEnd = min(o.End, readEnd+maxBatchBytes)
		}
	}
	return inserted, o.Cursor, nil, nil
}

func (r *ingestor) reportSource(path string, err error) {
	fmt.Fprintf(r.stderr, "ohya: %s: %v\n", path, err)
}

func (r *ingestor) reportMissing(o watch.Outcome) (missingAtStartup bool) {
	if r.missingReported[o.SourceID] {
		return false
	}
	r.missingReported[o.SourceID] = true
	if r.excludedPaths[o.Path] {
		fmt.Fprintf(r.stderr,
			"ohya: %s: source %s now matches --exclude and stops being captured going forward; the acknowledged checkpoint is kept and stored sessions stay on the hub (ohya purge removes them)\n",
			o.Path, o.SourceID)
		return false
	}
	r.event(observe.LevelWarn, observe.EventSourceMissing, "", nil)
	if !r.present[o.SourceID] {
		return true
	}
	fmt.Fprintf(r.stderr,
		"ohya: %s: source %s disappeared; the acknowledged checkpoint is kept and ingestion resumes if the file returns\n",
		o.Path, o.SourceID)
	return false
}

func (r *ingestor) reportMissingAtStartup(paths []string) {
	switch len(paths) {
	case 0:
	case 1:
		fmt.Fprintf(r.stderr, "ohya: %s: source with a saved checkpoint was already missing at startup; the acknowledged checkpoint is kept and ingestion resumes if the file returns\n", paths[0])
	default:
		fmt.Fprintf(r.stderr, "ohya: %d sources with saved checkpoints were already missing at startup (first: %s); the acknowledged checkpoints are kept and ingestion resumes if a file returns\n",
			len(paths), slices.Min(paths))
	}
}

func (r *ingestor) reportStop(o watch.Outcome, stop *events.StopError) {
	if off, ok := r.reported[o.SourceID]; ok && off == stop.Offset {
		return
	}
	r.reported[o.SourceID] = stop.Offset
	remedy := "; remedy: repair the record at this offset in the source file; ohya never skips bytes"
	if stop.RecordTooLarge {
		remedy = "; remedy: rerun with --max-record-bytes above the record to ingest past this offset"
	}
	fmt.Fprintf(r.stderr, "ohya: %s: %v%s (docs/adr/0004-stopped-source-recovery.md)\n", o.Path, stop, remedy)
}

func stopRemedy(recordTooLarge bool) string {
	if recordTooLarge {
		return "increase_max_record_bytes"
	}
	return "repair_source_file"
}

func discoverSessionFiles(dirs []agentDir, excludes excludeGlobs, allowEmpty bool) ([]string, []string, error) {
	return discoverAgentSessionFiles("", dirs, excludes, allowEmpty)
}

func discoverAgentSessionFiles(agent string, dirs []agentDir, excludes excludeGlobs, allowEmpty bool) ([]string, []string, error) {
	var files []string
	var excluded []string
	for _, d := range dirs {
		found, skipped, err := walkSessionFiles(agent, d.dir, excludes)
		if errors.Is(err, os.ErrNotExist) && (d.optional || allowEmpty) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		files = append(files, found...)
		excluded = append(excluded, skipped...)
	}
	return files, excluded, nil
}

func walkSessionFiles(agent, root string, excludes excludeGlobs) ([]string, []string, error) {
	var files []string
	var excluded []string
	dirCache := make(dirEvalCache)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("discover %s: %w", p, err)
		}
		if agent == antigravity.AgentName && d.IsDir() {
			switch d.Name() {
			case "chunks", "steps", "scratch", ".user_uploaded", "messages":
				return filepath.SkipDir
			}
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if agent == antigravity.AgentName {
			if d.Name() != "transcript_full.jsonl" || antigravity.ConversationIDFromPath(p) == "" {
				return nil
			}
		}
		match, err := excludes.matchRegular(p, dirCache)
		if err != nil {
			return err
		}
		if match {
			excluded = append(excluded, p)
			return nil
		}
		files = append(files, p)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("session directory %s does not exist: %w", root, fs.ErrNotExist)
	}
	if err != nil {
		return nil, nil, err
	}
	return files, excluded, nil
}

func excludedPathSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	dirCache := make(dirEvalCache)
	for _, p := range paths {
		for _, form := range dirCache.regularPathForms(p) {
			set[form] = true
		}
	}
	return set
}
