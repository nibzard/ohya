// Performance regression test for ingestion. Unlike the benchmarks, this
// runs in every `go test` pass: it fails only on order-of-magnitude
// regressions — an accidentally unindexed lookup or superlinear aggregate
// work — not on machine-speed drift, so the ceiling sits far above the
// measured time. The benchmarks (ingest_bench_test.go) carry the precise
// numbers; docs/performance.md records them.

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

// TestIngestPerformanceFloor imports a 24 MiB corpus through the full
// parse-and-commit path. Measured 2026-11-14 (12th-gen laptop CPU, WAL,
// synchronous FULL): about 3 s. The 30 s ceiling is roughly ten times that,
// so a healthy build never flakes while a superlinear write path (an
// unindexed per-record lookup over ~7,000 events) blows past it.
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
	if elapsed > 30*time.Second {
		t.Fatalf("ingesting %d files took %s (ceiling 30s; measured ~1.3s on the 2026-11-14 reference machine) — the write path likely regressed",
			len(files), elapsed)
	}
	t.Logf("ingested %d events from %d files in %s", inserted, len(files), elapsed)
}
