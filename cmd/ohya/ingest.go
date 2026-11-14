// Local ingestion: the vertical slice that wires the source watcher, the
// deterministic agent parsers and the transactional write path into one run.
// Each agent (Claude Code, Codex, Pi) has its own session directory, watcher
// and checkpoint file. One pass discovers every agent's session files, reads
// only the unacknowledged bytes, commits them through the single writer, and
// acknowledges the committed cursor in the local checkpoint. A crash between
// commit and acknowledgment replays safely: the write path validates
// replayed bytes and counts only new records.
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
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

// maxBatchBytes bounds the bytes of one ingestion batch, so importing a large
// history file stays within a predictable memory budget. A batch never splits
// a record: parsing retains an incomplete tail, the next batch resumes at the
// committed cursor, and one read may extend past this bound to the end of a
// single record above it (never past the configured record size limit).
const maxBatchBytes int64 = 8 << 20

// agentRoot is one agent's session directory to watch.
type agentRoot struct {
	agent string // parser and checkpoint name, for example "codex"
	dir   string
	flag  string // command-line flag that sets dir, for diagnostics
	// optional marks a default directory the user did not ask for: while it
	// is missing it holds no sessions, so an agent that is not installed
	// costs nothing, and its sessions are picked up once it appears. A
	// directory passed explicitly must exist.
	optional bool
}

// ingestOptions configures one ingest run.
type ingestOptions struct {
	dbPath         string        // hub database; created with its parent directory
	stateDir       string        // device identity and source checkpoints
	roots          []agentRoot   // session directories to watch, one per agent
	interval       time.Duration // polling interval in continuous mode
	once           bool          // run exactly one pass
	maxRecordBytes int64         // per-record bound for every parser; <= 0 selects their default
	stdout         io.Writer     // progress summaries
	stderr         io.Writer     // diagnostics
}

// runIngest imports the existing history of every agent once, then polls
// incrementally until the context is canceled (continuous mode) or after one
// pass (--once). It returns only on systemic failures — an unusable database
// or a lost checkpoint state needs recovery. A failing source stops that
// source with a diagnostic; the next sources still commit.
func runIngest(ctx context.Context, o ingestOptions) error {
	// Fail before creating anything: a wrong root must not leave a database
	// and a device identity behind.
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
		case !root.optional:
			return fmt.Errorf("%s session directory %s does not exist; pass -%s", root.agent, root.dir, root.flag)
		}
	}
	if present == 0 {
		var flags []string
		for _, root := range o.roots {
			flags = append(flags, "-"+root.flag)
		}
		return fmt.Errorf("no agent session directory exists; pass %s", strings.Join(flags, " or "))
	}
	if err := os.MkdirAll(filepath.Dir(o.dbPath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(o.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	hub, err := store.Open(ctx, o.dbPath)
	if err != nil {
		return err
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
			return err // includes watch.ErrStateLost: recovery, not a silent restart
		}
		runners[i] = newIngestor(hub, w, root.agent, deviceID, o.maxRecordBytes, o.stderr)
	}

	for pass := 1; ; pass++ {
		var stats passStats
		for i, r := range runners {
			s, err := r.poll(ctx, o.roots[i].dir, o.roots[i].optional)
			if err != nil {
				return err
			}
			stats.sources += s.sources
			stats.inserted += s.inserted
		}
		if o.once || pass == 1 || stats.inserted > 0 {
			fmt.Fprintf(o.stdout, "ohya: imported %d events from %d sources\n",
				stats.inserted, stats.sources)
		}
		if o.once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(o.interval):
		}
	}
}

// ingestor drives one agent's local ingestion. It is not safe for concurrent
// use; one goroutine drives it, like the database writer.
type ingestor struct {
	hub            *store.DB
	watcher        *watch.Watcher
	agent          string
	deviceID       string
	maxRecordBytes int64 // per-record bound; <= 0 selects the parser default
	stderr         io.Writer
	// reported remembers the stop offset of every diagnostic already written,
	// so continuous polling prints a persistent diagnostic once per offset
	// instead of once per poll.
	reported map[string]int64
}

func newIngestor(hub *store.DB, w *watch.Watcher, agent, deviceID string, maxRecordBytes int64, stderr io.Writer) *ingestor {
	return &ingestor{
		hub: hub, watcher: w, agent: agent, deviceID: deviceID,
		maxRecordBytes: maxRecordBytes, stderr: stderr, reported: map[string]int64{},
	}
}

// passStats summarizes one polling pass.
type passStats struct {
	sources  int // live sources observed
	inserted int // events newly inserted and committed
}

// poll discovers the session files under root, ingests every live source's
// pending bytes, and acknowledges the committed cursors. A missing optional
// root holds no session files.
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
			// A changed incarnation re-imports from byte zero under a new
			// source ID; history duplicates. New files stay silent: a first
			// import of a large projects directory must not print a line per
			// file.
			fmt.Fprintf(r.stderr, "ohya: %s: new source %s re-imports from byte zero: %s\n",
				o.Path, o.SourceID, o.Reason)
		}
		if err := r.hub.RegisterSource(ctx, o.SourceID, r.deviceID, r.agent, o.Path); err != nil {
			r.reportSource(o.Path, err)
			continue
		}
		if o.End <= o.Cursor {
			continue // nothing pending
		}
		inserted, committed, err := r.ingestSource(ctx, o)
		if err != nil {
			r.reportSource(o.Path, err)
			continue
		}
		stats.inserted += inserted
		if err := r.watcher.Acknowledge(o.Path, o.SourceID, committed); err != nil {
			r.reportSource(o.Path, err)
		}
	}
	return stats, nil
}

// ingestSource reads the outcome's pending bytes in bounded batches, parses
// the complete records, and commits them through the write path. It returns
// how many events were newly inserted and the committed byte cursor. It never
// acknowledges: the caller records the cursor, so a crash in between replays
// the same bytes safely on the next pass.
func (r *ingestor) ingestSource(ctx context.Context, o watch.Outcome) (int, int64, error) {
	inserted := 0
	// readEnd bounds the bytes of one read. A batch never splits a record,
	// so when the pending bytes hold one record larger than the batch bound,
	// the read extends to that record's end instead of stalling below it.
	// The growth is still bounded: the parser stops the source as soon as
	// the incomplete record exceeds the configured size limit.
	readEnd := min(o.End, o.Cursor+maxBatchBytes)
	for o.Cursor < o.End {
		chunk := o
		chunk.End = readEnd
		data, err := r.watcher.ReadPending(chunk)
		if err != nil {
			return inserted, o.Cursor, err
		}
		opts := events.Options{SourceScope: o.SourceID, MaxRecordBytes: r.maxRecordBytes}
		if chunk.Cursor > 0 {
			// Records past offset 0 may not name their session (Codex):
			// the committed first record does.
			st, err := r.hub.SourceState(ctx, o.SourceID)
			if err != nil {
				return inserted, o.Cursor, err
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
				return inserted, o.Cursor, err
			}
			inserted += result.Inserted
			o.Cursor = result.Cursor
			readEnd = min(o.End, o.Cursor+maxBatchBytes)
		}
		var stop *events.StopError
		if errors.As(perr, &stop) {
			// The valid prefix before the offending offset is committed; the
			// source stops there until the remedy on the diagnostic is
			// applied. Bytes are never skipped, so this pass must not read
			// past the stop.
			r.reportStop(o, stop)
			return inserted, o.Cursor, nil
		}
		if perr != nil {
			return inserted, o.Cursor, perr
		}
		if len(res.Events) == 0 {
			if chunk.End >= o.End {
				return inserted, o.Cursor, nil // one incomplete record still growing
			}
			// No complete record within a full read: one record exceeds the
			// batch bound. Extend the read to the record's end.
			readEnd = min(o.End, readEnd+maxBatchBytes)
		}
	}
	return inserted, o.Cursor, nil
}

// reportSource writes one source's failure. The source keeps its checkpoint
// and retries on the next pass; other sources are unaffected.
func (r *ingestor) reportSource(path string, err error) {
	fmt.Fprintf(r.stderr, "ohya: %s: %v\n", path, err)
}

// reportStop writes a source's stopping diagnostic once per offset. The text
// carries offsets, reasons and the remedy only, never transcript content.
// The remedy is the decision ADR 0004 owns: a size stop clears by rerunning
// with a larger --max-record-bytes, every other stop by repairing the file.
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

// discoverSessionFiles walks one agent's session directory and returns every
// session file. Claude Code main sessions and subagent transcripts are
// *.jsonl, while spilled tool output and memory files carry other
// extensions; Codex rollouts are rollout-*.jsonl under dated directories;
// Pi sessions are <timestamp>_<uuid>.jsonl under encoded-cwd directories. A
// missing root wraps fs.ErrNotExist.
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
