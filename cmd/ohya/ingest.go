// Local Claude Code ingestion: the vertical slice that wires the source
// watcher, the deterministic parser and the transactional write path into one
// run. One pass discovers the session files, reads only the unacknowledged
// bytes, commits them through the single writer, and acknowledges the
// committed cursor in the local checkpoint. A crash between commit and
// acknowledgment replays safely: the write path validates replayed bytes and
// counts only new records.
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

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

// maxBatchBytes bounds the bytes of one ingestion batch, so importing a large
// history file stays within a predictable memory budget. A batch never splits
// a record: parsing retains an incomplete tail, and the next batch resumes at
// the committed cursor.
const maxBatchBytes int64 = 8 << 20

// ingestOptions configures one ingest run.
type ingestOptions struct {
	dbPath     string        // hub database; created with its parent directory
	stateDir   string        // device identity and source checkpoints
	claudeRoot string        // Claude Code projects directory to watch
	interval   time.Duration // polling interval in continuous mode
	once       bool          // run exactly one pass
	stdout     io.Writer     // progress summaries
	stderr     io.Writer     // diagnostics
}

// runIngest imports the existing Claude Code history once, then polls
// incrementally until the context is canceled (continuous mode) or after one
// pass (--once). It returns only on systemic failures — an unusable database
// or a lost checkpoint state needs recovery. A failing source stops that
// source with a diagnostic; the next sources still commit.
func runIngest(ctx context.Context, o ingestOptions) error {
	// Fail before creating anything: a wrong -claude-root must not leave a
	// database and a device identity behind.
	if _, err := os.Stat(o.claudeRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("claude-code projects directory %s does not exist; pass -claude-root", o.claudeRoot)
		}
		return fmt.Errorf("stat %s: %w", o.claudeRoot, err)
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
	w, err := watch.New(o.stateDir, claudecode.AgentName, watch.Options{})
	if err != nil {
		return err // includes watch.ErrStateLost: recovery, not a silent restart
	}
	r := newIngestor(hub, w, deviceID, o.stderr)

	for pass := 1; ; pass++ {
		stats, err := r.poll(ctx, o.claudeRoot)
		if err != nil {
			return err
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
	hub      *store.DB
	watcher  *watch.Watcher
	deviceID string
	stderr   io.Writer
	// reported remembers the stop offset of every diagnostic already written,
	// so continuous polling prints a persistent diagnostic once per offset
	// instead of once per poll.
	reported map[string]int64
}

func newIngestor(hub *store.DB, w *watch.Watcher, deviceID string, stderr io.Writer) *ingestor {
	return &ingestor{hub: hub, watcher: w, deviceID: deviceID, stderr: stderr, reported: map[string]int64{}}
}

// passStats summarizes one polling pass.
type passStats struct {
	sources  int // live sources observed
	inserted int // events newly inserted and committed
}

// poll discovers the session files under root, ingests every live source's
// pending bytes, and acknowledges the committed cursors.
func (r *ingestor) poll(ctx context.Context, root string) (passStats, error) {
	candidates, err := discoverSessionFiles(root)
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
		if err := r.hub.RegisterSource(ctx, o.SourceID, r.deviceID, claudecode.AgentName, o.Path); err != nil {
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
	for o.Cursor < o.End {
		chunk := o
		chunk.End = min(o.End, o.Cursor+maxBatchBytes)
		data, err := r.watcher.ReadPending(chunk)
		if err != nil {
			return inserted, o.Cursor, err
		}
		res, perr := claudecode.Parse(data, chunk.Cursor, claudecode.Options{SourceScope: o.SourceID})
		if len(res.Records) > 0 {
			result, err := r.hub.Ingest(ctx, store.Batch{
				SourceID: o.SourceID,
				DeviceID: r.deviceID,
				Events:   events.FromClaudeCodeRecords(res.Records),
			})
			if err != nil {
				return inserted, o.Cursor, err
			}
			inserted += result.Inserted
			o.Cursor = result.Cursor
		}
		if perr != nil {
			// The valid prefix before the offending offset is committed; the
			// source stops there until the file is repaired. Bytes are never
			// skipped, so this pass must not read past the stop.
			r.reportStop(o, perr)
			return inserted, o.Cursor, nil
		}
		if len(res.Records) == 0 {
			return inserted, o.Cursor, nil // the pending bytes are one incomplete record
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
// carries offsets and reasons only, never transcript content.
func (r *ingestor) reportStop(o watch.Outcome, err error) {
	var perr *claudecode.ParseError
	if !errors.As(err, &perr) {
		return
	}
	if r.reported[o.SourceID] == perr.Offset {
		return
	}
	r.reported[o.SourceID] = perr.Offset
	fmt.Fprintf(r.stderr, "ohya: %s: %v\n", o.Path, perr)
}

// discoverSessionFiles walks the Claude Code projects directory and returns
// every session file: main sessions and subagent transcripts are *.jsonl;
// spilled tool output and memory files carry other extensions and stay out.
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
		return nil, fmt.Errorf("claude-code projects directory %s does not exist; pass -claude-root", root)
	}
	if err != nil {
		return nil, err
	}
	return files, nil
}
