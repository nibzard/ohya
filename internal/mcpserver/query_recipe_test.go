package mcpserver

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/store"
)

func openRecipeHub(t *testing.T) *Engine {
	t.Helper()
	hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	seed := []string{
		`INSERT INTO sessions(id,agent,device_id,native_id,cwd,last_event_at_ms)
		 VALUES('claude:dev1:s1','claude','dev1','s1','/repo',900000),
		       ('claude:dev1:s2','claude','dev1','s2','/repo',900000),
		       ('claude:dev1:s3','claude','dev1','s3','/repo',900000),
		       ('claude:dev1:s9','claude','dev1','s9','/other',900000)`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES
		 ('src-1','dev1','claude','/repo/1.jsonl'),
		 ('src-2','dev1','claude','/repo/2.jsonl'),
		 ('src-3','dev1','claude','/repo/3.jsonl'),
		 ('src-9','dev1','claude','/other/9.jsonl')`,
	}
	for i, q := range seed {
		if _, err := hub.Writer().Exec(q); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	nextEvent := int64(0)
	addEvents := func(session, source string, times ...int64) {
		for _, ms := range times {
			nextEvent++
			q := fmt.Sprintf(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json)
			 VALUES(%d,'%s','%s',%d,%d,'message','assistant','event at %d','{}')`,
				nextEvent, session, source, nextEvent*100, ms, ms)
			if _, err := hub.Writer().Exec(q); err != nil {
				t.Fatalf("seed event %d: %v", nextEvent, err)
			}
		}
	}
	addEvents("claude:dev1:s1", "src-1", 1000, 4000, 7000)
	addEvents("claude:dev1:s2", "src-2", 2000, 5000, 8000)
	addEvents("claude:dev1:s3", "src-3", 3000, 6000, 9000)
	addEvents("claude:dev1:s9", "src-9", 9999)
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	return NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1"})
}

const recipeQuery = `SELECT e.id, e.session_id, e.time_ms, e.kind, e.role, substr(e.text, 1, 300) AS excerpt
FROM events e WHERE e.id IN (
  SELECT ev.id FROM sessions s JOIN events_settled ev ON ev.session_id = s.id
  WHERE s.cwd = '/repo'
  ORDER BY ev.time_ms DESC LIMIT 5)
ORDER BY e.time_ms DESC`

func TestProjectEventsRecipePlan(t *testing.T) {
	e := openRecipeHub(t)
	res := mustQuery(t, e, "EXPLAIN QUERY PLAN "+recipeQuery)
	var plan []string
	for _, row := range res.Rows {
		if len(row) < 4 {
			t.Fatalf("plan row = %v", row)
		}
		detail, ok := row[3].(string)
		if !ok {
			t.Fatalf("plan detail = %v", row[3])
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "COVERING INDEX events_session_time") {
		t.Fatalf("recipe plan must sort on the covering events_session_time index, got:\n%s", joined)
	}
	if !strings.Contains(joined, "INTEGER PRIMARY KEY (rowid=?)") {
		t.Fatalf("recipe plan must fetch only the selected ids by rowid, got:\n%s", joined)
	}
	for _, line := range plan {
		if strings.Contains(line, "SCAN ev") {
			t.Fatalf("recipe plan must not scan the events table:\n%s", joined)
		}
	}
}

func TestProjectEventsRecipeEquivalence(t *testing.T) {
	e := openRecipeHub(t)
	natural := `SELECT e.id FROM events_settled e JOIN sessions s ON e.session_id = s.id
WHERE s.cwd = '/repo' ORDER BY e.time_ms DESC LIMIT 5`
	want := mustQuery(t, e, natural)
	got := mustQuery(t, e, recipeQuery)
	if len(want.Rows) == 0 || len(got.Rows) == 0 {
		t.Fatalf("both shapes must return rows: natural %d, recipe %d", len(want.Rows), len(got.Rows))
	}
	if len(want.Rows) != len(got.Rows) {
		t.Fatalf("natural returned %d rows, recipe %d", len(want.Rows), len(got.Rows))
	}
	for i := range want.Rows {
		if want.Rows[i][0] != got.Rows[i][0] {
			t.Fatalf("row %d: natural id %v, recipe id %v", i, want.Rows[i][0], got.Rows[i][0])
		}
	}
}
