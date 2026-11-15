package events

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/store"
)

type stageRun struct {
	name       string
	open       time.Duration
	register   time.Duration
	state      time.Duration
	parse      time.Duration
	ingest     time.Duration
	wall       time.Duration
	bytes      int64
	inserted   int
	sources    int
	batches    int
	parseCalls int
	walPeak    int64
	dbBytes    int64
	verifyNote string
}

func (r stageRun) mibPerSec(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return (float64(r.bytes) / (1 << 20)) / d.Seconds()
}

type stageDiagnostic struct {
	key   string
	label string
	apply func(ctx context.Context, hub *store.DB) (string, error)
}

func stageDiagnosticsFor(names []string) []stageDiagnostic {
	all := []stageDiagnostic{
		{
			key:   "sync-off",
			label: "synchronous=OFF (durability syncs removed)",
			apply: func(ctx context.Context, hub *store.DB) (string, error) {
				if _, err := hub.Writer().ExecContext(ctx, `PRAGMA synchronous=OFF`); err != nil {
					return "", err
				}
				var mode int
				if err := hub.Writer().QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode); err != nil {
					return "", err
				}
				return fmt.Sprintf("synchronous=%d", mode), nil
			},
		},
		{
			key:   "fts-off",
			label: "events_fts_insert trigger dropped (no FTS index maintenance)",
			apply: func(ctx context.Context, hub *store.DB) (string, error) {
				if _, err := hub.Writer().ExecContext(ctx, `DROP TRIGGER events_fts_insert`); err != nil {
					return "", err
				}
				var n int
				if err := hub.Writer().QueryRowContext(ctx,
					`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='events_fts_insert'`).Scan(&n); err != nil {
					return "", err
				}
				return fmt.Sprintf("trigger rows=%d", n), nil
			},
		},
		{
			key:   "no-checkpoint",
			label: "wal_autocheckpoint=0 (checkpoint work removed from commits)",
			apply: func(ctx context.Context, hub *store.DB) (string, error) {
				if _, err := hub.Writer().ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`); err != nil {
					return "", err
				}
				var pages int
				if err := hub.Writer().QueryRowContext(ctx, `PRAGMA wal_autocheckpoint`).Scan(&pages); err != nil {
					return "", err
				}
				return fmt.Sprintf("wal_autocheckpoint=%d", pages), nil
			},
		},
	}
	var out []stageDiagnostic
	for _, name := range names {
		for _, d := range all {
			if d.key == name {
				out = append(out, d)
			}
		}
	}
	return out
}

func stageEnvInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q is not a positive integer", name, v)
	}
	return n
}

func stageEnvList(name, fallback string) []string {
	v := os.Getenv(name)
	if v == "" {
		v = fallback
	}
	if v == "none" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func stageCorpora(t *testing.T, agent string, mib int) []fixgen.AgentFiles {
	t.Helper()
	switch agent {
	case "claude-code":
		return []fixgen.AgentFiles{{Agent: "claude-code", Files: corpus(mib)}}
	case "codex":
		return []fixgen.AgentFiles{{Agent: "codex", Files: corpusCodex(mib)}}
	case "pi":
		return []fixgen.AgentFiles{{Agent: "pi", Files: corpusPi(mib)}}
	case "mix":
		return corpusMix(mib)
	default:
		t.Fatalf("OHYA_STAGE_AGENT=%q is not one of claude-code, codex, pi, mix", agent)
		return nil
	}
}

func runStagedIngest(t *testing.T, name string, corpora []fixgen.AgentFiles,
	apply func(context.Context, *store.DB) (string, error)) stageRun {
	t.Helper()
	const maxBatchBytes = 8 << 20
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hub.db")

	var run stageRun
	run.name = name
	t0 := time.Now()
	hub, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	run.open = time.Since(t0)
	if apply != nil {
		if run.verifyNote, err = apply(ctx, hub); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	walDone := make(chan struct{})
	walPeakCh := make(chan int64, 1)
	go func() {
		var peak int64
		defer func() { walPeakCh <- peak }()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-walDone:
				return
			case <-tick.C:
				if fi, err := os.Stat(dbPath + "-wal"); err == nil && fi.Size() > peak {
					peak = fi.Size()
				}
			}
		}
	}()

	wallStart := time.Now()
	for _, af := range corpora {
		for j, f := range af.Files {
			sourceID := fmt.Sprintf("%s-%04d", af.Agent, j)
			run.bytes += int64(len(f.Data))
			run.sources++
			t1 := time.Now()
			if err := hub.RegisterSource(ctx, sourceID, "dev-bench", af.Agent, "/"+f.Path); err != nil {
				t.Fatal(err)
			}
			run.register += time.Since(t1)

			end := int64(len(f.Data))
			cursor, readEnd := int64(0), min(end, maxBatchBytes)
			for cursor < end {
				opts := Options{SourceScope: sourceID}
				if cursor > 0 {
					t2 := time.Now()
					st, err := hub.SourceState(ctx, sourceID)
					if err != nil {
						t.Fatal(err)
					}
					run.state += time.Since(t2)
					opts.FirstNative = st.FirstNativeID
				}
				t3 := time.Now()
				res, err := Parse(af.Agent, f.Data[cursor:readEnd], cursor, opts)
				if err != nil {
					t.Fatal(err)
				}
				run.parse += time.Since(t3)
				run.parseCalls++
				if len(res.Events) > 0 {
					t4 := time.Now()
					result, err := hub.Ingest(ctx, store.Batch{
						SourceID: sourceID, DeviceID: "dev-bench", Events: res.Events})
					if err != nil {
						t.Fatal(err)
					}
					run.ingest += time.Since(t4)
					run.batches++
					run.inserted += result.Inserted
					cursor = result.Cursor
					readEnd = min(end, cursor+maxBatchBytes)
				} else if readEnd < end {
					readEnd = min(end, readEnd+maxBatchBytes)
				} else {
					break
				}
			}
		}
	}
	run.wall = time.Since(wallStart)
	close(walDone)
	run.walPeak = <-walPeakCh
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dbPath); err == nil {
		run.dbBytes = fi.Size()
	}
	return run
}

func logStageRun(t *testing.T, r stageRun) {
	t.Helper()
	committed := r.batches + r.sources
	mib := float64(r.bytes) / (1 << 20)
	t.Logf("%s: wall=%.2fs bytes=%.1fMiB events=%d sources=%d batches=%d commits=%d",
		r.name, r.wall.Seconds(), mib, r.inserted, r.sources, r.batches, committed)
	if r.verifyNote != "" {
		t.Logf("%s: diagnostic verified: %s", r.name, r.verifyNote)
	}
	t.Logf("%s: open+migrate=%.2fs register=%.2fs state-reads=%.2fs parse=%.2fs (%.0f MiB/s) ingest-writer=%.2fs (%.2f MiB/s)",
		r.name, r.open.Seconds(), r.register.Seconds(), r.state.Seconds(), r.parse.Seconds(),
		r.mibPerSec(r.parse), r.ingest.Seconds(), r.mibPerSec(r.ingest))
	t.Logf("%s: wal-peak=%.1fMiB db-after-close=%.1fMiB",
		r.name, float64(r.walPeak)/(1<<20), float64(r.dbBytes)/(1<<20))
}

func TestIngestStageAttribution(t *testing.T) {
	if os.Getenv("OHYA_STAGE_MIB") == "" {
		t.Skip("set OHYA_STAGE_MIB to run the ingestion stage attribution harness")
	}
	mib := stageEnvInt(t, "OHYA_STAGE_MIB", 32)
	agent := os.Getenv("OHYA_STAGE_AGENT")
	if agent == "" {
		agent = "claude-code"
	}
	repeat := stageEnvInt(t, "OHYA_STAGE_REPEAT", 1)

	corpora := stageCorpora(t, agent, mib)
	files := 0
	for _, af := range corpora {
		files += len(af.Files)
	}
	t.Logf("corpus: agent=%s scale=%dMiB files=%d", agent, mib, files)

	runs := map[string]stageRun{}
	var baseline stageRun
	for i := 0; i < repeat; i++ {
		name := fmt.Sprintf("baseline #%d", i+1)
		run := runStagedIngest(t, name, corpora, nil)
		logStageRun(t, run)
		baseline = run
	}
	runs["baseline"] = baseline
	base := baseline.wall

	for _, diag := range stageDiagnosticsFor(stageEnvList("OHYA_STAGE_DIAG", "sync-off,fts-off,no-checkpoint")) {
		run := runStagedIngest(t, diag.label, corpora, diag.apply)
		logStageRun(t, run)
		runs[diag.key] = run
		t.Logf("%s: delta vs baseline: wall %+.2fs", run.name, run.wall.Seconds()-base.Seconds())
	}

	if syncOff, ok := runs["sync-off"]; ok {
		ckpt, hasCkpt := runs["no-checkpoint"]
		if hasCkpt {
			t.Logf("derived: durable-sync wait (baseline - sync-off) = %.2fs; commit-only wait (no-checkpoint - sync-off) = %.2fs; checkpoint share (baseline - no-checkpoint) = %.2fs",
				base.Seconds()-syncOff.wall.Seconds(),
				ckpt.wall.Seconds()-syncOff.wall.Seconds(),
				base.Seconds()-ckpt.wall.Seconds())
		} else {
			t.Logf("derived: durable-sync wait (baseline - sync-off) = %.2fs",
				base.Seconds()-syncOff.wall.Seconds())
		}
	}
}
