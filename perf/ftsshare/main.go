package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nibzard/ohya/perf/hubdata"

	_ "github.com/mattn/go-sqlite3"
)

type category struct {
	name      string
	table     string
	docs      int64
	unindexed int64
	textBytes int64
	indexByte int64
}

func main() {
	dbPath := flag.String("db", filepath.Join(homeDir(), ".ohya", "hub.db"), "hub database (opened read-only)")
	sample := flag.Int("sample", 20, "take every Nth event id")
	scratch := flag.String("scratch", "", "scratch directory for the measurement database (default: fresh temp dir)")
	out := flag.String("out", "perf/t102/fts-share.txt", "output file (aggregate numbers only)")
	flag.Parse()

	started := time.Now()
	if err := run(*dbPath, *sample, *scratch, *out, started); err != nil {
		fmt.Fprintln(os.Stderr, "ftsshare:", err)
		os.Exit(1)
	}
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

func run(dbPath string, sample int, scratch, out string, started time.Time) error {
	if sample < 1 {
		return fmt.Errorf("-sample must be >= 1")
	}
	hub, err := hubdata.OpenReadOnly(dbPath)
	if err != nil {
		return err
	}
	defer hub.Close()

	if scratch == "" {
		prefix := os.Getenv("LOOPER_RESOURCE_PREFIX")
		if prefix == "" {
			prefix = "ohya"
		}
		scratch, err = os.MkdirTemp("", prefix+"-ftsshare-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(scratch)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return err
	}
	measure, err := sql.Open("sqlite3", filepath.Join(scratch, "measure.db"))
	if err != nil {
		return err
	}
	defer measure.Close()
	if _, err := measure.Exec("PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF"); err != nil {
		return err
	}

	tables := newTableSet(measure)
	toolOut := tables.mustGet("tool outputs", "toolout")
	toolRest := tables.mustGet("everything else", "toolrest")
	all := tables.mustGet("all docs", "alldocs")

	if err := scanEvents(hub, tables, sample, toolOut, toolRest, all); err != nil {
		return err
	}
	if err := tables.measureIndexes(); err != nil {
		return err
	}

	list := tables.sorted()
	report(out, sample, list, toolOut, toolRest, all, started)
	return nil
}

type tableSet struct {
	db     *sql.DB
	cats   []*category
	byName map[string]*category
}

func newTableSet(db *sql.DB) *tableSet {
	return &tableSet{db: db, byName: map[string]*category{}}
}

func (t *tableSet) mustGet(name, table string) *category {
	c := &category{name: name, table: table}
	if err := t.create(c); err != nil {
		panic(err)
	}
	t.cats = append(t.cats, c)
	t.byName[name] = c
	return c
}

func (t *tableSet) get(name string) (*category, error) {
	if c, ok := t.byName[name]; ok {
		return c, nil
	}
	c := &category{name: name, table: "c" + strconv.Itoa(len(t.cats))}
	if err := t.create(c); err != nil {
		return nil, err
	}
	t.cats = append(t.cats, c)
	t.byName[name] = c
	return c, nil
}

func (t *tableSet) create(c *category) error {
	_, err := t.db.Exec("CREATE VIRTUAL TABLE " + c.table + " USING fts5(text, content='')")
	return err
}

func (t *tableSet) insert(c *category, id int64, text string) error {
	c.docs++
	c.textBytes += int64(len(text))
	_, err := t.db.Exec("INSERT INTO "+c.table+"(rowid, text) VALUES (?, ?)", id, text)
	return err
}

func (t *tableSet) sorted() []*category {
	out := make([]*category, 0, len(t.cats))
	for _, c := range t.cats {
		if _, ok := map[string]bool{"tool outputs": true, "everything else": true, "all docs": true}[c.name]; ok {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].indexByte > out[j].indexByte })
	return out
}

func (t *tableSet) measureIndexes() error {
	for _, c := range t.cats {
		if c.docs == 0 {
			continue
		}
		if _, err := t.db.Exec("INSERT INTO " + c.table + "(" + c.table + ") VALUES ('optimize')"); err != nil {
			return err
		}
	}
	rows, err := t.db.Query("SELECT name, sum(pgsize) FROM dbstat GROUP BY name")
	if err != nil {
		return fmt.Errorf("dbstat (build with -tags sqlite_dbstat): %w", err)
	}
	defer rows.Close()
	perTable := map[string]int64{}
	for rows.Next() {
		var name string
		var pgsize int64
		if err := rows.Scan(&name, &pgsize); err != nil {
			return err
		}
		perTable[name] = pgsize
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range t.cats {
		if c.docs == 0 {
			continue
		}
		c.indexByte = perTable[c.table+"_data"] + perTable[c.table+"_docsize"] + perTable[c.table+"_idx"]
	}
	return nil
}

func scanEvents(hub *sql.DB, tables *tableSet, sample int, toolOut, toolRest, all *category) error {
	rows, err := hub.Query(`
		SELECT e.id, coalesce(e.text, ''), s.agent, e.kind,
		       coalesce(e.role, ''), coalesce(e.tool_name, ''),
		       CASE WHEN s.agent = 'claude-code' THEN e.metadata ELSE '' END,
		       CASE WHEN s.agent = 'codex' AND e.role IS NULL AND e.tool_name IS NULL
		            THEN coalesce(json_extract(e.raw_json, '$.payload.type'), '') ELSE '' END
		FROM events e JOIN sessions s ON s.id = e.session_id
		WHERE e.id % ? = 0`, sample)
	if err != nil {
		return err
	}
	defer rows.Close()

	var scanned int64
	var lastReport int64
	for rows.Next() {
		var id int64
		var text, agent, kind, role, toolName, metadata, payloadType string
		if err := rows.Scan(&id, &text, &agent, &kind, &role, &toolName, &metadata, &payloadType); err != nil {
			return err
		}
		scanned++
		name := agent + "/" + kind
		if role != "" {
			name += "/" + role
		}
		c, err := tables.get(name)
		if err != nil {
			return err
		}
		if len(text) == 0 {
			c.unindexed++
			continue
		}
		if err := tables.insert(c, id, text); err != nil {
			return err
		}
		part := toolRest
		if hubdata.ToolOutput(agent, kind, role, toolName, payloadType, metadata) {
			part = toolOut
		}
		if err := tables.insert(part, id, text); err != nil {
			return err
		}
		if err := tables.insert(all, id, text); err != nil {
			return err
		}
		if scanned-lastReport >= 20000 {
			lastReport = scanned
			fmt.Fprintf(os.Stderr, "scanned %d sampled events (%.1f MB text indexed)\n", scanned, float64(all.textBytes)/1e6)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "scanned %d sampled events in total (%.1f MB text indexed)\n", scanned, float64(all.textBytes)/1e6)
	return nil
}

func report(out string, sample int, list []*category, toolOut, toolRest, all *category, started time.Time) {
	var sumCats int64
	for _, c := range list {
		sumCats += c.indexByte
	}
	sumParts := toolOut.indexByte + toolRest.indexByte

	var b strings.Builder
	fmt.Fprintf(&b, "FTS index share by event category\n")
	fmt.Fprintf(&b, "sampled: every %dth event id; each category measured in its own contentless FTS5 table\n", sample)
	fmt.Fprintf(&b, "index bytes = fts5 _data + _docsize + _idx pages after optimize\n\n")

	fmt.Fprintf(&b, "%-42s %8s %10s %10s %10s %9s\n", "category", "docs", "text MB", "index KB", "idx share", "idx/text")
	for _, c := range list {
		fmt.Fprintf(&b, "%-42s %8d %10.1f %10.0f %9.1f%% %9.3f\n",
			c.name, c.docs, float64(c.textBytes)/1e6, float64(c.indexByte)/1e3,
			shareOf(c.indexByte, sumCats)*100, indexPerText(c))
	}

	fmt.Fprintf(&b, "\nunindexed sampled rows (empty text):\n")
	for _, c := range list {
		if c.unindexed > 0 {
			fmt.Fprintf(&b, "  %-42s %d of %d sampled rows\n", c.name, c.unindexed, c.unindexed+c.docs)
		}
	}

	fmt.Fprintf(&b, "\ntool-output partition (plan 004, question for O6)\n")
	fmt.Fprintf(&b, "%-42s %8s %10s %10s %10s %9s\n", "partition", "docs", "text MB", "index KB", "idx share", "idx/text")
	for _, c := range []*category{toolOut, toolRest} {
		fmt.Fprintf(&b, "%-42s %8d %10.1f %10.0f %9.1f%% %9.3f\n",
			c.name, c.docs, float64(c.textBytes)/1e6, float64(c.indexByte)/1e3,
			shareOf(c.indexByte, sumParts)*100, indexPerText(c))
	}

	fmt.Fprintf(&b, "\nadditivity: sum(categories)=%.0f KB, tool partition=%.0f KB, all-docs index=%.0f KB\n",
		float64(sumCats)/1e3, float64(sumParts)/1e3, float64(all.indexByte)/1e3)
	fmt.Fprintf(&b, "sum(categories)/all = %.3f, tool partition/all = %.3f\n",
		ratio(sumCats, all.indexByte), ratio(sumParts, all.indexByte))
	fmt.Fprintf(&b, "all-docs index/text = %.3f (hub reference 2026-11-16: 0.83 GB index over about 2.7 GB non-empty text = 0.30)\n",
		indexPerText(all))
	fmt.Fprintf(&b, "elapsed: %s\n", time.Since(started).Round(time.Second))

	text := b.String()
	fmt.Print(text)
	if out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
			_ = os.WriteFile(out, []byte(text), 0o644)
		}
	}
}

func shareOf(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func indexPerText(c *category) float64 {
	if c.textBytes == 0 {
		return 0
	}
	return float64(c.indexByte) / float64(c.textBytes)
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
