package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
	"github.com/nibzard/ohya/internal/workspace"
)

const maxBatchBytes int64 = 8 << 20

type agentRoot struct {
	agent    string
	dir      string
	flag     string
	optional bool
}

type ingestOptions struct {
	dbPath         string
	stateDir       string
	roots          []agentRoot
	interval       time.Duration
	allowEmpty     bool
	once           bool
	maxRecordBytes int64
	jsonOut        bool
	stdout         io.Writer
	stderr         io.Writer
}

var errDegraded = errors.New("ingest completed with a failed or stopped source")

func runIngest(ctx context.Context, o ingestOptions) error {
	if len(o.roots) == 0 {
		return errors.New("no agent session directory to watch")
	}
	present := 0
	for _, root := range o.roots {
		if !events.Supported(root.agent) {
			return fmt.Errorf("agent %q has no parser", root.agent)
		}
		_, err := os.Stat(root.dir)
		switch {
		case err == nil:
			present++
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("stat %s: %w", root.dir, err)
		case !root.optional && !o.allowEmpty:
			return classify(fmt.Errorf("%s session directory %s does not exist; pass --%s", root.agent, root.dir, root.flag),
				"missing_agent_root", "pass the named --<agent>-root flag, or create the directory")
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
	hub, err := store.Open(ctx, o.dbPath)
	if err != nil {
		return classify(err, "database_open_failed", "check the --db path and its permissions; another ohya process may hold the database")
	}
	defer hub.Close()

	deviceID, err := device.LoadOrCreate(o.stateDir)
	if err != nil {
		return err
	}
	runners := make([]*ingestor, len(o.roots))
	for i, root := range o.roots {
		w, err := watch.New(o.stateDir, root.agent, watch.Options{})
		if err != nil {
			return err
		}
		runners[i] = newIngestor(hub, w, root.agent, deviceID, o.maxRecordBytes, o.stderr)
	}

	for pass := 1; ; pass++ {
		var stats passStats
		for i, r := range runners {
			s, err := r.poll(ctx, o.roots[i].dir, o.roots[i].optional || o.allowEmpty)
			if err != nil {
				return err
			}
			stats.sources += s.sources
			stats.inserted += s.inserted
			stats.failed += s.failed
			stats.stopped += s.stopped
			stats.stops = append(stats.stops, s.stops...)
		}
		switch {
		case o.jsonOut:
			stops := stats.stops
			if stops == nil {
				stops = []stopReport{}
			}
			if err := printJSON(o.stdout, ingestSpec.name, ingestJSONData{
				Events:  stats.inserted,
				Sources: stats.sources,
				Failed:  stats.failed,
				Stopped: stats.stopped,
				Stops:   stops,
			}); err != nil {
				return err
			}
		case o.once || pass == 1 || stats.inserted > 0:
			summary := fmt.Sprintf("ohya: imported %d events from %d sources", stats.inserted, stats.sources)
			if stats.failed > 0 || stats.stopped > 0 {
				summary += fmt.Sprintf("; %d failed, %d stopped", stats.failed, stats.stopped)
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
	hub            *store.DB
	watcher        *watch.Watcher
	agent          string
	deviceID       string
	maxRecordBytes int64
	stderr         io.Writer
	reported       map[string]int64
	workspacesDue  bool
}

func newIngestor(hub *store.DB, w *watch.Watcher, agent, deviceID string, maxRecordBytes int64, stderr io.Writer) *ingestor {
	return &ingestor{
		hub: hub, watcher: w, agent: agent, deviceID: deviceID,
		maxRecordBytes: maxRecordBytes, stderr: stderr, reported: map[string]int64{},
		workspacesDue: true,
	}
}

type passStats struct {
	sources  int
	inserted int
	failed   int
	stopped  int
	stops    []stopReport
}

type stopReport struct {
	SourceID string `json:"source_id"`
	Path     string `json:"path"`
	Offset   int64  `json:"offset"`
	Remedy   string `json:"remedy"`
}

type ingestJSONData struct {
	Events  int          `json:"events"`
	Sources int          `json:"sources"`
	Failed  int          `json:"failed"`
	Stopped int          `json:"stopped"`
	Stops   []stopReport `json:"stops"`
}

func (r *ingestor) poll(ctx context.Context, root string, optional bool) (passStats, error) {
	candidates, err := discoverSessionFiles(root)
	if optional && errors.Is(err, fs.ErrNotExist) {
		candidates, err = nil, nil
	}
	if err != nil {
		return passStats{}, err
	}
	outcomes, err := r.watcher.Poll(candidates)
	if err != nil {
		return passStats{}, err
	}
	var stats passStats
	for _, o := range outcomes {
		if o.Missing {
			continue
		}
		stats.sources++
		if o.Fresh && o.Reason != "new file" {
			fmt.Fprintf(r.stderr, "ohya: %s: new source %s re-imports from byte zero: %s\n",
				o.Path, o.SourceID, o.Reason)
		}
		if err := r.hub.RegisterSource(ctx, o.SourceID, r.deviceID, r.agent, o.Path); err != nil {
			r.reportSource(o.Path, err)
			stats.failed++
			continue
		}
		if o.End <= o.Cursor {
			continue
		}
		inserted, committed, stop, err := r.ingestSource(ctx, o)
		if err != nil {
			r.reportSource(o.Path, err)
			stats.failed++
			continue
		}
		if stop != nil {
			stats.stopped++
			stats.stops = append(stats.stops, stopReport{
				SourceID: o.SourceID,
				Path:     o.Path,
				Offset:   stop.Offset,
				Remedy:   stopRemedy(stop.RecordTooLarge),
			})
		}
		stats.inserted += inserted
		if err := r.watcher.Acknowledge(o.Path, o.SourceID, committed); err != nil {
			r.reportSource(o.Path, err)
			stats.failed++
		}
	}
	if r.workspacesDue || stats.inserted > 0 {
		r.observeWorkspaces(ctx)
	}
	return stats, nil
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
		fmt.Fprintf(r.stderr, "ohya: observe %s workspaces, retrying next pass: %v\n", r.agent, err)
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
			return inserted, o.Cursor, nil, err
		}
		opts := events.Options{SourceScope: o.SourceID, MaxRecordBytes: r.maxRecordBytes}
		if chunk.Cursor > 0 {
			st, err := r.hub.SourceState(ctx, o.SourceID)
			if err != nil {
				return inserted, o.Cursor, nil, err
			}
			opts.FirstNative = st.FirstNativeID
		}
		res, perr := events.Parse(r.agent, data, chunk.Cursor, opts)
		if len(res.Events) > 0 {
			result, err := r.hub.Ingest(ctx, store.Batch{
				SourceID: o.SourceID,
				DeviceID: r.deviceID,
				Events:   res.Events,
			})
			if err != nil {
				return inserted, o.Cursor, nil, err
			}
			inserted += result.Inserted
			o.Cursor = result.Cursor
			readEnd = min(o.End, o.Cursor+maxBatchBytes)
		}
		var stop *events.StopError
		if errors.As(perr, &stop) {
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

func discoverSessionFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("discover %s: %w", p, err)
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, p)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("session directory %s does not exist: %w", root, fs.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	return files, nil
}
