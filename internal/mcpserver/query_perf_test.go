// Performance regression test for retrieval through the sql tool's engine.
// Like the ingestion floor test, it fails only on order-of-magnitude
// regressions, not on machine-speed drift: each query must answer well
// inside the engine's 5 s deadline on a seeded hub. The benchmarks
// (query_bench_test.go) carry the precise numbers; docs/performance.md
// records them.

package mcpserver

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/store"
)

// TestRetrievalPerformanceCeiling seeds a 12 MiB hub and runs the three
// patterns agents rely on most: wide full-text search (the most expensive
// ranked query), session token totals, and annotation paging. Measured
// 2026-11-14: all under 10 ms. The 2 s ceiling sits far above that; the
// engine's 5 s deadline is the hard bound an agent actually experiences.
func TestRetrievalPerformanceCeiling(t *testing.T) {
	hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	ctx := context.Background()
	files := fixgen.Generate(fixgen.Corpus{
		Sessions:            4,
		MainMeanBytes:       2 << 20,
		SubagentsPerSession: 2,
		Seed:                7,
	})
	for j, f := range files {
		sourceID := fmt.Sprintf("src-%03d", j)
		if err := hub.RegisterSource(ctx, sourceID, "dev-perf",
			claudecode.AgentName, "/"+f.Path); err != nil {
			t.Fatal(err)
		}
		res, err := events.Parse(claudecode.AgentName, f.Data, 0,
			events.Options{SourceScope: sourceID})
		if err != nil {
			t.Fatalf("%s: parse: %v", f.Path, err)
		}
		if _, err := hub.Ingest(ctx, store.Batch{
			SourceID: sourceID, DeviceID: "dev-perf", Events: res.Events}); err != nil {
			t.Fatalf("%s: ingest: %v", f.Path, err)
		}
	}
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer readers.Close()
	engine := NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev-perf"})

	queries := map[string]string{
		"fts-wide": `SELECT e.id, substr(e.text, 1, 200)
			FROM events_fts JOIN events e ON e.id = events_fts.rowid
			WHERE events_fts MATCH 'session' ORDER BY events_fts.rank LIMIT 10`,
		"session-token-totals": `SELECT s.id, sum(u.input_tokens), sum(u.output_tokens)
			FROM sessions s LEFT JOIN session_token_usage u ON u.session_id = s.id
			GROUP BY s.id`,
		"annotation-paging": `SELECT e.id FROM events e
			WHERE e.id > 0 AND NOT EXISTS (
				SELECT 1 FROM event_annotations a
				WHERE a.event_id = e.id AND a.writer_id = 'dev-perf'
					AND a.namespace = 'security' AND a.key = 'review')
			ORDER BY e.id LIMIT 100`,
	}
	const ceiling = 2 * time.Second
	for name, query := range queries {
		start := time.Now()
		result, err := engine.Query(ctx, query)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.RowCount == 0 {
			t.Fatalf("%s: returned no rows; the seeded corpus must produce output", name)
		}
		if elapsed > ceiling {
			t.Fatalf("%s took %s (ceiling 2s; measured <10ms on the 2026-11-14 reference machine) — the query path likely regressed",
				name, elapsed)
		}
		t.Logf("%s: %d rows in %s", name, result.RowCount, elapsed)
	}
}
