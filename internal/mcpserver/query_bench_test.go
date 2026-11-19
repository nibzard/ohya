package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

var benchTelemetry struct {
	once     sync.Once
	observer observe.Observer
	usage    UsageSink
	dir      string
}

func benchEngineOptions() EngineOptions {
	benchTelemetry.once.Do(func() {
		mode := os.Getenv("OHYA_BENCH_OBSERVE")
		if mode != "on" && mode != "metrics" {
			return
		}
		clock := observe.NewSystemClock()
		instance, err := observe.NewInstance("bench", "", false, clock, observe.RoleMCP)
		if err != nil {
			panic(err)
		}
		recorder, err := observe.NewRecorder(instance, clock)
		if err != nil {
			panic(err)
		}
		benchTelemetry.observer = recorder
		if mode == "on" {
			dir, err := os.MkdirTemp("", "ohya-query-bench-observe-")
			if err != nil {
				panic(err)
			}
			usage, err := observe.NewUsageWriter(observe.UsageWriterConfig{
				Dir:      filepath.Join(dir, "observe"),
				Instance: instance,
				Clock:    clock,
			})
			if err != nil {
				panic(err)
			}
			benchTelemetry.usage = usage
			benchTelemetry.dir = dir
		}
	})
	return EngineOptions{WriterID: "dev-bench", Observer: benchTelemetry.observer, Usage: benchTelemetry.usage}
}

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

func benchFixture() string {
	return os.Getenv("OHYA_BENCH_FIXTURE")
}

func seedBenchHub() (*store.DB, *Engine, string, error) {
	if benchFixture() == "query" {
		return seedQueryFixtureHub()
	}
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
	return finishBenchHub(hub, dir)
}

func seedQueryFixtureHub() (*store.DB, *Engine, string, error) {
	dir, err := os.MkdirTemp("", "ohya-query-fixture-")
	if err != nil {
		return nil, nil, "", err
	}
	hub, err := store.Open(context.Background(), filepath.Join(dir, "hub.db"))
	if err != nil {
		return nil, nil, "", err
	}
	corpora := fixgen.GenerateMix(fixgen.QueryMix(7))
	ctx := context.Background()
	for _, af := range corpora {
		for j, f := range af.Files {
			sourceID := fmt.Sprintf("%s-%04d", af.Agent, j)
			if err := hub.RegisterSource(ctx, sourceID, "dev-bench", af.Agent, "/"+f.Path); err != nil {
				return nil, nil, "", err
			}
			res, err := events.Parse(af.Agent, f.Data, 0, events.Options{SourceScope: sourceID})
			if err != nil {
				return nil, nil, "", err
			}
			if _, err := hub.Ingest(ctx, store.Batch{
				SourceID: sourceID, DeviceID: "dev-bench", Events: res.Events}); err != nil {
				return nil, nil, "", err
			}
		}
	}
	return finishBenchHub(hub, dir)
}

func finishBenchHub(hub *store.DB, dir string) (*store.DB, *Engine, string, error) {
	readers, err := hub.Reader()
	if err != nil {
		return nil, nil, "", err
	}
	return hub, NewEngine(readers, hub.Writer(), benchEngineOptions()), dir, nil
}

func acquireBenchEngine(tb testing.TB) *Engine {
	tb.Helper()
	benchHub.once.Do(func() {
		benchHub.db, benchHub.eng, benchHub.dir, benchHub.err = seedBenchHub()
	})
	if benchHub.err != nil {
		tb.Fatal(benchHub.err)
	}
	return benchHub.eng
}

func TestMain(m *testing.M) {
	code := m.Run()
	if benchHub.db != nil {
		benchHub.db.Close()
	}
	if benchHub.dir != "" {
		os.RemoveAll(benchHub.dir)
	}
	if benchTelemetry.usage != nil {
		if w, ok := benchTelemetry.usage.(*observe.UsageWriter); ok {
			w.Close()
		}
	}
	if benchTelemetry.dir != "" {
		os.RemoveAll(benchTelemetry.dir)
	}
	os.Exit(code)
}

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

var fixedQueryWorkload = []struct {
	name  string
	query string
}{
	{"FTS-selective", `SELECT e.id, substr(e.text, 1, 200)
		FROM events_fts JOIN events e ON e.id = events_fts.rowid
		WHERE events_fts MATCH 'authentication' ORDER BY events_fts.rank LIMIT 10`},
	{"FTS-wide", `SELECT e.id, substr(e.text, 1, 200)
		FROM events_fts JOIN events e ON e.id = events_fts.rowid
		WHERE events_fts MATCH 'session' ORDER BY events_fts.rank LIMIT 10`},
	{"session-token-totals", `SELECT s.id, s.first_prompt,
			sum(u.input_tokens) AS input_tokens, sum(u.output_tokens) AS output_tokens
		FROM sessions s
		LEFT JOIN session_token_usage u ON u.session_id = s.id
		GROUP BY s.id`},
	{"recent-sessions", `SELECT id, agent, first_prompt, event_count
		FROM sessions ORDER BY last_event_at_ms DESC LIMIT 20`},
	{"session-event-range", `SELECT e.id, e.time_ms, substr(e.text, 1, 200)
		FROM events e WHERE e.session_id =
			(SELECT id FROM sessions ORDER BY last_event_at_ms DESC LIMIT 1)
			AND e.time_ms >= 0
		ORDER BY e.time_ms LIMIT 500`},
	{"annotation-paging", `SELECT e.id FROM events e
		WHERE e.id > 0 AND NOT EXISTS (
			SELECT 1 FROM event_annotations a
			WHERE a.event_id = e.id AND a.writer_id = 'dev-bench'
				AND a.namespace = 'security' AND a.key = 'review')
		ORDER BY e.id LIMIT 100`},
}

func BenchmarkQuery(b *testing.B) {
	for _, q := range fixedQueryWorkload {
		queryBench(b, q.name, q.query)
	}
}

func queryPct(sorted []time.Duration, pct int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (pct*len(sorted) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	return sorted[idx-1]
}

func TestQueryFixturePercentiles(t *testing.T) {
	if benchFixture() != "query" {
		t.Skip("set OHYA_BENCH_FIXTURE=query to measure the one-million-event fixture")
	}
	e := acquireBenchEngine(t)
	ctx := context.Background()
	if rows, err := e.Query(ctx, "SELECT count(*) AS n FROM events"); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("fixture hub events: %v", rows)
	}
	const warmup = 10
	samples := 150
	if v := os.Getenv("OHYA_QUERY_SAMPLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			samples = n
		}
	}
	for _, q := range fixedQueryWorkload {
		for i := 0; i < warmup; i++ {
			if _, err := e.Query(ctx, q.query); err != nil {
				t.Fatalf("%s: %v", q.name, err)
			}
		}
		durs := make([]time.Duration, 0, samples)
		var errCount, timeouts int
		for i := 0; i < samples; i++ {
			start := time.Now()
			_, err := e.Query(ctx, q.query)
			dur := time.Since(start)
			if err != nil {
				errCount++
				if strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "timeout") {
					timeouts++
				}
				continue
			}
			durs = append(durs, dur)
		}
		sort.Slice(durs, func(a, b int) bool { return durs[a] < durs[b] })
		if len(durs) == 0 {
			t.Errorf("%s: every query failed", q.name)
			continue
		}
		t.Logf("%s: n=%d/%d p50=%s p95=%s p99=%s max=%s errors=%d timeouts=%d",
			q.name, len(durs), samples, queryPct(durs, 50).Round(time.Millisecond),
			queryPct(durs, 95).Round(time.Millisecond), queryPct(durs, 99).Round(time.Millisecond),
			durs[len(durs)-1].Round(time.Millisecond), errCount, timeouts)
		if errCount > 0 {
			t.Errorf("%s: %d of %d queries failed (%d deadlines)", q.name, errCount, samples, timeouts)
		}
	}
}
