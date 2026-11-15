package events

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/store"
)

func TestIngestPerformanceFloor(t *testing.T) {
	files := fixgen.Generate(fixgen.Corpus{
		Sessions:            8,
		MainMeanBytes:       2 << 20,
		SubagentsPerSession: 2,
		Seed:                7,
	})
	hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	ctx := context.Background()
	start := time.Now()
	inserted := 0
	for j, f := range files {
		sourceID := fmt.Sprintf("src-%03d", j)
		if err := hub.RegisterSource(ctx, sourceID, "dev-perf",
			claudecode.AgentName, "/"+f.Path); err != nil {
			t.Fatal(err)
		}
		res, err := Parse(claudecode.AgentName, f.Data, 0,
			Options{SourceScope: sourceID})
		if err != nil {
			t.Fatalf("%s: parse: %v", f.Path, err)
		}
		result, err := hub.Ingest(ctx, store.Batch{
			SourceID: sourceID, DeviceID: "dev-perf", Events: res.Events})
		if err != nil {
			t.Fatalf("%s: ingest: %v", f.Path, err)
		}
		inserted += result.Inserted
	}
	elapsed := time.Since(start)
	if inserted == 0 {
		t.Fatal("corpus inserted no events")
	}
	ceiling := 120 * time.Second
	if elapsed > ceiling {
		t.Fatalf("ingesting %d files took %s (ceiling %s with a contention allowance; quiet runs measure 1.3-3 s — docs/performance.md, Regression guards) — the write path likely regressed",
			len(files), elapsed, ceiling)
	}
	t.Logf("ingested %d events from %d files in %s", inserted, len(files), elapsed)
}
