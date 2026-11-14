// Retrieval benchmarks over a seeded synthetic hub (internal/fixgen). They
// measure the sql tool's real path — Engine.Query with its authorizer,
// one-statement scanner, deadline and row/byte limits — so the numbers
// answer what an agent's query costs at a given corpus volume. The companion
// measurements and method live in docs/performance.md.

package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/store"
)

// benchHub lazily seeds one shared hub for all query benchmarks: ingesting
// the corpus per benchmark would dominate the run. TestMain closes it and
// removes its directory.
var benchHub struct {
	once sync.Once
	db   *store.DB
	eng  *Engine
	dir  string
	err  error
}

func benchMIB() int {
	var n int
	if v := os.Getenv("OHYA_BENCH_MIB"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 32
}

// seedBenchHub ingests the benchmark corpus (default 32 MiB, OHYA_BENCH_MIB
// to raise) and builds the engine with the tool's default limits. The
// caller owns the returned directory.
func seedBenchHub() (*store.DB, *Engine, string, error) {
	dir, err := os.MkdirTemp("", "ohya-query-bench-")
	if err != nil {
		return nil, nil, "", err
	}
	hub, err := store.Open(context.Background(), filepath.Join(dir, "hub.db"))
	if err != nil {
		return nil, nil, "", err
	}
	files := fixgen.Generate(fixgen.Corpus{
		Sessions:            benchMIB() / 3,
		MainMeanBytes:       2 << 20,
		SubagentsPerSession: 2,
		Seed:                7,
	})
	ctx := context.Background()
	for j, f := range files {
		sourceID := fmt.Sprintf("src-%03d", j)
		if err := hub.RegisterSource(ctx, sourceID, "dev-bench",
			claudecode.AgentName, "/"+f.Path); err != nil {
			return nil, nil, "", err
		}
		res, err := events.Parse(claudecode.AgentName, f.Data, 0,
			events.Options{SourceScope: sourceID})
		if err != nil {
			return nil, nil, "", err
		}
		if _, err := hub.Ingest(ctx, store.Batch{
			SourceID: sourceID, DeviceID: "dev-bench", Events: res.Events}); err != nil {
			return nil, nil, "", err
		}
	}
	readers, err := hub.Reader()
	if err != nil {
		return nil, nil, "", err
	}
	return hub, NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev-bench"}), dir, nil
}

func acquireBenchEngine(b *testing.B) *Engine {
	b.Helper()
	benchHub.once.Do(func() {
		benchHub.db, benchHub.eng, benchHub.dir, benchHub.err = seedBenchHub()
	})
	if benchHub.err != nil {
		b.Fatal(benchHub.err)
	}
	return benchHub.eng
}

// TestMain closes the shared benchmark hub after all benchmarks ran and
// removes its directory; a benchmark-local cleanup would close it while
// later benchmarks still use it.
func TestMain(m *testing.M) {
	code := m.Run()
	if benchHub.db != nil {
		benchHub.db.Close()
	}
	if benchHub.dir != "" {
		os.RemoveAll(benchHub.dir)
	}
	os.Exit(code)
}

// queryBench runs one query repeatedly and reports per-call latency.
func queryBench(b *testing.B, name, query string) {
	b.Run(name, func(b *testing.B) {
		e := acquireBenchEngine(b)
		ctx := context.Background()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := e.Query(ctx, query); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkQuery runs the retrieval patterns agents use (DB.md documents
// them): selective and wide full-text search, session token totals, the
// recent-sessions listing, an event range within one session, and the
// annotation paging query.
func BenchmarkQuery(b *testing.B) {
	queryBench(b, "FTS-selective", `SELECT e.id, substr(e.text, 1, 200)
		FROM events_fts JOIN events e ON e.id = events_fts.rowid
		WHERE events_fts MATCH 'authentication' ORDER BY events_fts.rank LIMIT 10`)
	queryBench(b, "FTS-wide", `SELECT e.id, substr(e.text, 1, 200)
		FROM events_fts JOIN events e ON e.id = events_fts.rowid
		WHERE events_fts MATCH 'session' ORDER BY events_fts.rank LIMIT 10`)
	queryBench(b, "session-token-totals", `SELECT s.id, s.first_prompt,
			sum(u.input_tokens) AS input_tokens, sum(u.output_tokens) AS output_tokens
		FROM sessions s
		LEFT JOIN session_token_usage u ON u.session_id = s.id
		GROUP BY s.id`)
	queryBench(b, "recent-sessions", `SELECT id, agent, first_prompt, event_count
		FROM sessions ORDER BY last_event_at_ms DESC LIMIT 20`)
	queryBench(b, "session-event-range", `SELECT e.id, e.time_ms, substr(e.text, 1, 200)
		FROM events e WHERE e.session_id =
			(SELECT id FROM sessions ORDER BY last_event_at_ms DESC LIMIT 1)
			AND e.time_ms >= 0
		ORDER BY e.time_ms LIMIT 500`)
	queryBench(b, "annotation-paging", `SELECT e.id FROM events e
		WHERE e.id > 0 AND NOT EXISTS (
			SELECT 1 FROM event_annotations a
			WHERE a.event_id = e.id AND a.writer_id = 'dev-bench'
				AND a.namespace = 'security' AND a.key = 'review')
		ORDER BY e.id LIMIT 100`)
}
