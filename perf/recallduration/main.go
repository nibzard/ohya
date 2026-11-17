package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

type cwdGroup struct {
	cwd    string
	sess   int64
	events int64
	newest int64
	oldest int64
}

type sample struct {
	totalMS float64
	outcome string
	rows    int64
	bytes   int64
}

type sink struct {
	records []observe.UsageRecord
}

func (s *sink) Record(r observe.UsageRecord) bool {
	s.records = append(s.records, r)
	return true
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	idx := int(q * float64(len(sorted)-1))
	return sorted[idx]
}

func summarize(name string, samples []sample) string {
	var totals []float64
	timeouts := 0
	rows := int64(0)
	bytes := int64(0)
	max := 0.0
	for _, s := range samples {
		totals = append(totals, s.totalMS)
		if s.outcome == observe.UsageOutcomeTimeout {
			timeouts++
		}
		if s.totalMS > max {
			max = s.totalMS
		}
		rows = s.rows
		bytes = s.bytes
	}
	return fmt.Sprintf("%-26s n=%3d cold=%8.1fms p50=%8.1fms p95=%8.1fms max=%8.1fms timeouts=%d rows=%d result=%dB",
		name, len(samples), samples[0].totalMS, quantile(totals, 0.5), quantile(totals, 0.95),
		max, timeouts, rows, bytes)
}

func evictCache(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
		f.Close()
	}
}

func main() {
	dbPath := flag.String("db", "", "fixture hub database")
	mode := flag.String("mode", "measure", "build | measure | index")
	scale := flag.Float64("scale", 2.8, "mix scale for build (compact fixture)")
	seed := flag.Int64("seed", 7, "corpus seed")
	runs := flag.Int("runs", 40, "warm runs per shape")
	flag.Parse()
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "recallduration: -db is required")
		os.Exit(1)
	}
	ctx := context.Background()
	switch *mode {
	case "build":
		build(ctx, *dbPath, *scale, *seed)
	case "measure":
		measure(ctx, *dbPath, *runs)
	case "index":
		indexExperiment(ctx, *dbPath, *runs)
	default:
		fmt.Fprintf(os.Stderr, "recallduration: unknown mode %q\n", *mode)
		os.Exit(1)
	}
}

func build(ctx context.Context, dbPath string, scale float64, seed int64) {
	mix := fixgen.Mix{Seed: seed, Scale: scale, Compact: true}
	corpora := fixgen.GenerateMix(mix)
	hub, err := store.Open(ctx, dbPath)
	if err != nil {
		panic(err)
	}
	defer hub.Close()
	start := time.Now()
	files := 0
	var eventsTotal int64
	for _, af := range corpora {
		for j, f := range af.Files {
			sourceID := fmt.Sprintf("t104-%s-%05d", af.Agent, j)
			if err := hub.RegisterSource(ctx, sourceID, "t104-bench", af.Agent, "/"+f.Path); err != nil {
				panic(err)
			}
			res, err := events.Parse(af.Agent, f.Data, 0, events.Options{SourceScope: sourceID})
			if err != nil {
				panic(fmt.Sprintf("%s: %v", f.Path, err))
			}
			ing, err := hub.Ingest(ctx, store.Batch{
				SourceID: sourceID, DeviceID: "t104-bench", Events: res.Events})
			if err != nil {
				panic(fmt.Sprintf("%s: %v", f.Path, err))
			}
			files++
			eventsTotal += int64(ing.Inserted)
			if files%2000 == 0 {
				fmt.Printf("recallduration: %d files, %d events, %s elapsed\n", files, eventsTotal, time.Since(start).Round(time.Second))
			}
		}
	}
	fmt.Printf("recallduration: built %s: %d files, %d events in %s\n",
		dbPath, files, eventsTotal, time.Since(start).Round(time.Second))
}

func measure(ctx context.Context, dbPath string, runs int) {
	hub, err := store.OpenReadOnly(dbPath)
	if err != nil {
		panic(err)
	}
	readers, err := hub.Reader()
	if err != nil {
		panic(err)
	}
	defer readers.Close()

	var total struct {
		events   int64
		sessions int64
		sources  int64
	}
	if err := readers.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM sessions), (SELECT count(*) FROM sources)`).
		Scan(&total.events, &total.sessions, &total.sources); err != nil {
		panic(err)
	}
	fmt.Printf("recallduration: fixture %s: %d events, %d sessions, %d sources\n",
		dbPath, total.events, total.sessions, total.sources)

	groups := readGroups(ctx, readers)
	biggest := groups[0]
	var median cwdGroup
	if len(groups) > 0 {
		median = groups[len(groups)/2]
	}
	fmt.Printf("recallduration: cwd groups: %d; largest %s (%d sessions, %d events), median %s (%d events)\n",
		len(groups), biggest.cwd, biggest.sess, biggest.events, median.cwd, median.events)

	s := &sink{}
	engine := mcpserver.NewEngine(readers, nil, mcpserver.EngineOptions{
		WriterID: "t104-bench",
		Usage:    s,
	})

	shapes := []struct {
		name string
		sql  func(cwd string) string
	}{
		{"s0-join-events", s0Join("events")},
		{"s0-join-settled", s0Join("events_settled")},
		{"s1-recipe-topk", s1Recipe},
		{"s2-recipe-bounded", s2Bounded},
	}
	for _, target := range []struct {
		label string
		group cwdGroup
	}{{"largest", biggest}, {"median", median}} {
		for _, shape := range shapes {
			evictCache(dbPath)
			s.records = s.records[:0]
			samples := runShape(ctx, engine, s, shape.sql(target.group.cwd), runs)
			fmt.Printf("%-8s %s\n", target.label, summarize(shape.name, samples))
		}
	}
	fmt.Println("recallduration: plans")
	for _, shape := range shapes {
		res, err := engine.Query(ctx, "EXPLAIN QUERY PLAN "+shape.sql(biggest.cwd))
		if err != nil {
			fmt.Printf("%s: plan error: %v\n", shape.name, err)
			continue
		}
		fmt.Printf("%s:\n", shape.name)
		for _, row := range res.Rows {
			fmt.Printf("   %v\n", row)
		}
	}
}

func readGroups(ctx context.Context, readers *sql.DB) []cwdGroup {
	rows, err := readers.QueryContext(ctx, `SELECT s.cwd, count(*), sum(s.event_count), max(s.last_event_at_ms), min(s.started_at_ms)
		FROM sessions s WHERE s.cwd IS NOT NULL GROUP BY s.cwd ORDER BY sum(s.event_count) DESC`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	var out []cwdGroup
	for rows.Next() {
		var g cwdGroup
		var newest, oldest sql.NullInt64
		if err := rows.Scan(&g.cwd, &g.sess, &g.events, &newest, &oldest); err != nil {
			panic(err)
		}
		g.newest, g.oldest = newest.Int64, oldest.Int64
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		panic(err)
	}
	return out
}

func runShape(ctx context.Context, engine *mcpserver.Engine, s *sink, query string, runs int) []sample {
	var out []sample
	for i := 0; i < runs; i++ {
		s.records = s.records[:0]
		result, err := engine.Query(ctx, query)
		if err != nil && i == 0 {
			fmt.Printf("   first run error: %v\n", err)
		}
		for _, r := range s.records {
			out = append(out, sample{
				totalMS: r.PhaseMS.TotalMS,
				outcome: r.Outcome,
				rows:    r.Rows,
				bytes:   r.ResultBytes,
			})
		}
		_ = result
	}
	return out
}

func s0Join(table string) func(string) string {
	return func(cwd string) string {
		return fmt.Sprintf(`SELECT e.id, e.session_id, e.time_ms, e.kind, e.role, e.tool_name, substr(e.text, 1, 300) AS text_preview
FROM events e JOIN sessions s ON e.session_id = s.id
WHERE s.cwd = '%s'
ORDER BY e.time_ms DESC LIMIT 5`, cwd)
	}
}

func s1Recipe(cwd string) string {
	return fmt.Sprintf(`SELECT e.id, e.session_id, e.time_ms, e.kind, e.role, e.tool_name, substr(e.text, 1, 300) AS text_preview
FROM events e WHERE e.id IN (
  SELECT ev.id FROM sessions s JOIN events_settled ev ON ev.session_id = s.id
  WHERE s.cwd = '%s'
  ORDER BY ev.time_ms DESC LIMIT 5)
ORDER BY e.time_ms DESC`, cwd)
}

func s2Bounded(cwd string) string {
	return fmt.Sprintf(`SELECT e.id, e.session_id, e.time_ms, e.kind, e.role, e.tool_name, substr(e.text, 1, 300) AS text_preview
FROM events e JOIN sessions s ON e.session_id = s.id
WHERE s.cwd = '%s' AND e.time_ms > (SELECT max(last_event_at_ms) - 604800000 FROM sessions)
ORDER BY e.time_ms DESC LIMIT 5`, cwd)
}

func indexExperiment(ctx context.Context, dbPath string, runs int) {
	hub, err := store.Open(ctx, dbPath)
	if err != nil {
		panic(err)
	}
	readers, err := hub.Reader()
	if err != nil {
		panic(err)
	}
	s := &sink{}
	engine := mcpserver.NewEngine(readers, hub.Writer(), mcpserver.EngineOptions{
		WriterID: "t104-bench",
		Usage:    s,
	})
	var pageSize, before int
	if err := hub.Writer().QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		panic(err)
	}
	if err := hub.Writer().QueryRowContext(ctx, "PRAGMA page_count").Scan(&before); err != nil {
		panic(err)
	}
	groups := readGroups(ctx, readers)
	biggest := groups[0]
	missing := "/home/agent/no-such-project"

	start := time.Now()
	if _, err := hub.Writer().ExecContext(ctx, "CREATE INDEX events_time_id ON events(time_ms DESC, id)"); err != nil {
		panic(err)
	}
	buildWall := time.Since(start)
	var after int
	if err := hub.Writer().QueryRowContext(ctx, "PRAGMA page_count").Scan(&after); err != nil {
		panic(err)
	}
	fmt.Printf("recallduration: index experiment: CREATE INDEX events_time_id (time_ms DESC, id) took %s, added %d pages (%.1f MiB)\n",
		buildWall.Round(time.Millisecond), after-before, float64(after-before)*float64(pageSize)/(1<<20))

	for _, probe := range []struct {
		label string
		cwd   string
	}{{"largest", biggest.cwd}, {"missing", missing}} {
		evictCache(dbPath)
		s.records = s.records[:0]
		samples := runShape(ctx, engine, s, s0Join("events")(probe.cwd), runs)
		fmt.Printf("%-8s %s\n", probe.label, summarize("s0-join-with-index", samples))
		res, err := engine.Query(ctx, "EXPLAIN QUERY PLAN "+s0Join("events")(probe.cwd))
		if err == nil {
			fmt.Printf("%s plan:\n", probe.label)
			for _, row := range res.Rows {
				fmt.Printf("   %v\n", row)
			}
		}
	}

	if _, err := hub.Writer().ExecContext(ctx, "DROP INDEX events_time_id"); err != nil {
		panic(err)
	}
	if _, err := hub.Writer().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		panic(err)
	}
	var restored int
	if err := hub.Writer().QueryRowContext(ctx, "PRAGMA page_count").Scan(&restored); err != nil {
		panic(err)
	}
	fmt.Printf("recallduration: index dropped; page_count restored to %d (was %d before)\n", restored, before)
	hub.Close()
}
