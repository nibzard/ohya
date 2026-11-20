package mcpserver

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/store"
)

const publishedSQLBlockCount = 12

var sqlBlockOpeners = []string{"SELECT ", "WITH ", "PRAGMA ", "EXPLAIN ", "VALUES "}

func startsSQLStatement(line string) bool {
	for _, opener := range sqlBlockOpeners {
		if strings.HasPrefix(line, opener) {
			return true
		}
	}
	return false
}

func publishedSQLBlocks(t *testing.T, description string) []string {
	t.Helper()
	var blocks []string
	for _, para := range strings.Split(description, "\n\n") {
		lines := strings.Split(para, "\n")
		for i, line := range lines {
			if startsSQLStatement(line) {
				blocks = append(blocks, strings.TrimSpace(strings.Join(lines[i:], "\n")))
				break
			}
		}
	}
	if len(blocks) != publishedSQLBlockCount {
		t.Fatalf("extracted %d SQL blocks from the sql tool description, want %d; update publishedSQLBlockCount after a deliberate example change", len(blocks), publishedSQLBlockCount)
	}
	return blocks
}

func openPublishedRecipesHub(t *testing.T) *Engine {
	t.Helper()
	hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	now := time.Now().UnixMilli()
	const hour, day = int64(3600_000), int64(86400_000)
	settled1, settled2, live, old := now-hour, now-2*hour, now, now-30*day
	seed := []string{
		fmt.Sprintf(`INSERT INTO sessions(id,agent,device_id,native_id,repo,cwd,model,first_prompt,last_event_at_ms) VALUES
		 ('claude:dev1:s1','claude','dev1','s1','github.com/owner/name','/repo','claude-sonnet-4-5','settled s1',%d),
		 ('claude:dev1:s2','claude','dev1','s2','github.com/owner/name','/repo',NULL,'settled s2',%d),
		 ('claude:dev1:s-old','claude','dev1','s-old','github.com/owner/name','/repo',NULL,'old session',%d),
		 ('claude:dev1:s-live','claude','dev1','s-live','github.com/owner/name','/repo',NULL,'live session',%d),
		 ('claude:laptop:s-lap','claude','laptop','s-lap','github.com/owner/demo','/demo','claude-sonnet-4-5','laptop session',%d)`,
			settled1, settled2, old, live, now-3*hour),
		`INSERT INTO sources(id,device_id,agent,path) VALUES
		 ('src-1','dev1','claude','/repo/1.jsonl'),
		 ('src-2','dev1','claude','/repo/2.jsonl'),
		 ('src-3','dev1','claude','/repo/3.jsonl'),
		 ('src-live','dev1','claude','/repo/live.jsonl')`,
		fmt.Sprintf(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json) VALUES
		 (1,'claude:dev1:s1','src-1',100,%d,'message','user','first s1 record','{}'),
		 (2,'claude:dev1:s1','src-1',200,%d,'message','assistant','hello authentication world, refresh the token','{}'),
		 (3,'claude:dev1:s1','src-1',300,%d,'message','assistant','third s1 record','{}'),
		 (4,'claude:dev1:s2','src-2',100,%d,'message','user','first s2 record','{}'),
		 (5,'claude:dev1:s2','src-2',200,%d,'message','assistant','second s2 record','{}'),
		 (6,'claude:dev1:s-live','src-live',100,%d,'message','user','live session record','{}'),
		 (7,'claude:dev1:s-old','src-3',100,%d,'message','user','thirty day old record','{}'),
		 (42,'claude:dev1:s-old','src-3',200,%d,'message','assistant','inspect me by id','{}'),
		 (100,'claude:dev1:s-old','src-3',300,%d,'message','assistant','reviewed already','{}'),
		 (101,'claude:dev1:s-old','src-3',400,%d,'message','assistant','needs review','{}'),
		 (102,'claude:dev1:s-old','src-3',500,%d,'message','assistant','also needs review','{}')`,
			settled1+1000, settled1+2000, settled1+3000, settled2, settled2+1000, live,
			old, old+1000, old+2000, old+3000, old+4000),
		`INSERT INTO session_token_usage VALUES('claude:dev1:s1','msg-1',110,20,30,40)`,
		`INSERT INTO event_annotations VALUES(100,'security','review','{"status":"reviewed"}','dev1',1,1)`,
	}
	for i, q := range seed {
		if _, err := hub.Writer().Exec(q); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	return NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1"})
}

func TestPublishedRecipesRunVerbatim(t *testing.T) {
	e := openPublishedRecipesHub(t)
	blocks := publishedSQLBlocks(t, ToolDescription(EngineOptions{WriterID: "dev1"}))
	ran := make(map[string]Result, len(blocks))
	for _, block := range blocks {
		res := mustQuery(t, e, block)
		if res.Truncated {
			t.Fatalf("published recipe truncated (%s):\n%s", res.TruncationReason, block)
		}
		if res.RowCount == 0 {
			t.Fatalf("published recipe returned no rows over the seeded hub:\n%s", block)
		}
		ran[block] = res
	}
	find := func(marker string) Result {
		var matches []Result
		for _, block := range blocks {
			if strings.Contains(block, marker) {
				matches = append(matches, ran[block])
			}
		}
		if len(matches) != 1 {
			t.Fatalf("marker %q matched %d published blocks, want exactly 1", marker, len(matches))
		}
		return matches[0]
	}
	eventIDs := func(res Result) []int64 {
		ids := make([]int64, 0, len(res.Rows))
		for _, row := range res.Rows {
			id, ok := row[0].(int64)
			if !ok {
				t.Fatalf("first result column = %T (%v), want an event id", row[0], row[0])
			}
			ids = append(ids, id)
		}
		return ids
	}

	dateBounded := eventIDs(find("strftime('%s','now')"))
	slices.Sort(dateBounded)
	if !slices.Equal(dateBounded, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("date-bounded recipe = %v, want the five events inside the window and none of the 30-day-old ones", dateBounded)
	}

	resume1 := find("AND EXISTS (SELECT 1 FROM events_settled")
	if len(resume1.Rows) != 1 || resume1.Rows[0][0] != "claude:dev1:s1" {
		t.Fatalf("resume step 1 = %v, want the settled session claude:dev1:s1 over the newer live one", resume1.Rows)
	}

	resume2 := eventIDs(find("duplicate_of IS NULL"))
	if !slices.Equal(resume2, []int64{1, 2, 3}) {
		t.Fatalf("resume step 2 = %v, want s1's three records oldest-first", resume2)
	}

	fts := eventIDs(find("events_fts MATCH"))
	if !slices.Equal(fts, []int64{2}) {
		t.Fatalf("full-text recipe = %v, want only the event seeded with both terms", fts)
	}

	var modelSessions []string
	for _, row := range find("s.model = 'claude-sonnet-4-5'").Rows {
		modelSessions = append(modelSessions, row[0].(string))
	}
	slices.Sort(modelSessions)
	if !slices.Equal(modelSessions, []string{"claude:dev1:s1", "claude:laptop:s-lap"}) {
		t.Fatalf("model recipe = %v, want exactly the two seeded claude-sonnet-4-5 sessions", modelSessions)
	}

	device := find("s.device_id = 'laptop'")
	if len(device.Rows) != 1 || device.Rows[0][0] != "claude:laptop:s-lap" {
		t.Fatalf("device recipe = %v, want exactly the seeded laptop session", device.Rows)
	}

	inspect := eventIDs(find("WHERE id = 42"))
	if !slices.Equal(inspect, []int64{42}) {
		t.Fatalf("inspect-by-id recipe = %v, want only event 42", inspect)
	}

	review := eventIDs(find("json_extract(a.value"))
	if !slices.Equal(review, []int64{101, 102}) {
		t.Fatalf("security-review recipe = %v, want the two unreviewed events 101 and 102", review)
	}
}
