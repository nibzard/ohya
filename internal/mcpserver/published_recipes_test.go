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

const (
	toolDescriptionSQLBlockCount = 11
	garbageGuideSQLBlockCount    = 8
	analyticsGuideSQLBlockCount  = 7
)

var sqlBlockOpeners = []string{"SELECT ", "WITH ", "PRAGMA ", "EXPLAIN ", "VALUES "}

func startsSQLStatement(line string) bool {
	for _, opener := range sqlBlockOpeners {
		if strings.HasPrefix(line, opener) {
			return true
		}
	}
	return false
}

func publishedSQLBlocks(t *testing.T, label, text string, wantCount int) []string {
	t.Helper()
	var blocks []string
	for _, para := range strings.Split(text, "\n\n") {
		lines := strings.Split(para, "\n")
		for i, line := range lines {
			if startsSQLStatement(line) {
				blocks = append(blocks, strings.TrimSpace(strings.Join(lines[i:], "\n")))
				break
			}
		}
	}
	if len(blocks) != wantCount {
		t.Fatalf("extracted %d SQL blocks from %s, want %d; update the count pin after a deliberate example change", len(blocks), label, wantCount)
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
	fresh, stale := now-60_000, now-40*day
	digestAt := now - 10*day
	oversizedRaw := `{"type":"user","sessionId":"s-old","message":{"role":"user","content":"` + strings.Repeat("x", 70000) + `"}}`
	seed := []string{
		fmt.Sprintf(`INSERT INTO sessions(id,agent,device_id,native_id,repo,cwd,model,first_prompt,last_event_at_ms,event_count) VALUES
		 ('claude-code:dev1:s1','claude-code','dev1','s1','github.com/owner/name','/repo','claude-sonnet-4-5','settled s1',%d,3),
		 ('claude-code:dev1:s2','claude-code','dev1','s2','github.com/owner/name','/repo',NULL,'settled s2',%d,2),
		 ('claude-code:dev1:s-old','claude-code','dev1','s-old','github.com/owner/name','/repo',NULL,'old session',%d,6),
		 ('claude-code:dev1:s-live','claude-code','dev1','s-live','github.com/owner/name','/repo',NULL,'live session',%d,1),
		 ('claude-code:laptop:s-lap','claude-code','laptop','s-lap','github.com/owner/demo','/demo','claude-sonnet-4-5','laptop session',%d,0),
		 ('claude-code:dev1:s-gone','claude-code','dev1','s-gone',NULL,'/gone','claude-3-opus-20240229','gone session',%d,0)`,
			settled1, settled2, old, live, now-3*hour, old),
		fmt.Sprintf(`INSERT INTO sources(id,device_id,agent,path,cursor_bytes,updated_at_ms) VALUES
		 ('src-1','dev1','claude-code','/repo/1.jsonl',1000,%d),
		 ('src-2','dev1','claude-code','/repo/2.jsonl',500,%d),
		 ('src-3','dev1','claude-code','/repo/3.jsonl',9000,%d),
		 ('src-live','dev1','claude-code','/repo/live.jsonl',100,%d)`,
			fresh, fresh, stale, fresh),
		fmt.Sprintf(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json) VALUES
		 (1,'claude-code:dev1:s1','src-1',100,%d,'message','user','first s1 record','{}'),
		 (2,'claude-code:dev1:s1','src-1',200,%d,'message','assistant','hello authentication world, refresh the token','{}'),
		 (3,'claude-code:dev1:s1','src-1',300,%d,'message','assistant','third s1 record','{}'),
		 (4,'claude-code:dev1:s2','src-2',100,%d,'message','user','first s2 record','{}'),
		 (5,'claude-code:dev1:s2','src-2',200,%d,'message','assistant','second s2 record','{}'),
		 (6,'claude-code:dev1:s-live','src-live',100,%d,'message','user','live session record','{}'),
		 (7,'claude-code:dev1:s-old','src-3',100,%d,'message','user','thirty day old record','{}'),
		 (42,'claude-code:dev1:s-old','src-3',200,%d,'message','assistant','inspect me by id','{}'),
		 (50,'claude-code:dev1:s-old','src-3',600,%d,'message','user',NULL,'%s'),
		 (100,'claude-code:dev1:s-old','src-3',300,%d,'message','assistant','reviewed already','{}'),
		 (101,'claude-code:dev1:s-old','src-3',400,%d,'message','assistant','needs review','{}'),
		 (102,'claude-code:dev1:s-old','src-3',500,%d,'message','assistant','also needs review','{}')`,
			settled1+1000, settled1+2000, settled1+3000, settled2, settled2+1000, live,
			old, old+1000, old+5000, oversizedRaw, old+2000, old+3000, old+4000),
		fmt.Sprintf(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json,duplicate_of) VALUES
		 (51,'claude-code:dev1:s2','src-2',300,%d,'message','user',NULL,'{"type":"user","sessionId":"s2"}',4)`, old),
		fmt.Sprintf(`INSERT INTO sessions(id,agent,device_id,native_id,repo,cwd,branch,first_prompt,started_at_ms,last_event_at_ms,event_count) VALUES
		 ('claude-code:dev1:s-digest','claude-code','dev1','s-digest','github.com/owner/name','/repo','feature/digest','add the digest',%d,%d,7)`, digestAt, digestAt+6000),
		fmt.Sprintf(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,tool_name,text,metadata,raw_json) VALUES
		 (60,'claude-code:dev1:s-digest','src-3',1000,%d,'user','user',NULL,'add the digest','{"blocks":[{"type":"text"}]}','{}'),
		 (61,'claude-code:dev1:s-digest','src-3',1100,%d,'assistant','assistant','Bash','Bash {"command":"ls"}','{}','{}'),
		 (62,'claude-code:dev1:s-digest','src-3',1200,%d,'user','user',NULL,'ls output','{"blocks":[{"type":"tool_result"}]}','{}'),
		 (63,'claude-code:dev1:s-digest','src-3',1300,%d,'assistant','assistant',NULL,'digest added','{}','{}'),
		 (64,'claude-code:dev1:s-digest','src-3',1400,%d,'user','user',NULL,'now ship it','{"blocks":[{"type":"text"}]}','{}'),
		 (65,'claude-code:dev1:s-digest','src-3',1500,%d,'assistant','assistant',NULL,'shipped','{}','{}')`,
			digestAt, digestAt+1000, digestAt+2000, digestAt+3000, digestAt+4000, digestAt+5000),
		fmt.Sprintf(`INSERT INTO workspaces(device_id,path,repo,observed_at_ms) VALUES('dev1','/gone',NULL,%d)`, old),
		`INSERT INTO session_token_usage VALUES('claude-code:dev1:s1','msg-1',110,20,30,40)`,
		`INSERT INTO event_annotations VALUES(100,'security','review','{"status":"reviewed"}','dev1',1,1)`,
		`INSERT INTO event_annotations VALUES(7,'tags','garbage','{"reason":"stale model session","evidence":7}','dev1',1,1)`,
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

func TestToolDescriptionStatesFtsTermQuoting(t *testing.T) {
	var ftsExample string
	for _, para := range strings.Split(ToolDescription(EngineOptions{WriterID: "dev1"}), "\n\n") {
		if strings.Contains(para, "events_fts MATCH") {
			ftsExample = para
			break
		}
	}
	if ftsExample == "" {
		t.Fatal("tool description lacks the events_fts MATCH example")
	}
	for _, want := range []string{"double quotes", `'"auth-refresh"'`} {
		if !strings.Contains(ftsExample, want) {
			t.Fatalf("full-text search guidance lacks %q:\n%s", want, ftsExample)
		}
	}
}

func TestPublishedRecipesRunVerbatim(t *testing.T) {
	e := openPublishedRecipesHub(t)
	opts := EngineOptions{WriterID: "dev1"}
	var blocks []string
	blocks = append(blocks, publishedSQLBlocks(t, "sql tool description", ToolDescription(opts), toolDescriptionSQLBlockCount)...)
	blocks = append(blocks, publishedSQLBlocks(t, GuideGarbageAuditURI, GarbageAuditGuide(), garbageGuideSQLBlockCount)...)
	blocks = append(blocks, publishedSQLBlocks(t, GuideAnalyticsReviewURI, AnalyticsReviewGuide(opts), analyticsGuideSQLBlockCount)...)
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

	digest := find("SUM(kind = 'user') OVER")
	if digest.RowCount != 2 {
		t.Fatalf("digest rows = %d (%v), want 2 exchanges", digest.RowCount, digest.Rows)
	}
	if digest.Rows[0][1] != int64(60) || digest.Rows[0][3] != int64(63) || digest.Rows[1][1] != int64(64) || digest.Rows[1][3] != int64(65) {
		t.Fatalf("digest = %v, want user 60 with reply 63 and user 64 with reply 65", digest.Rows)
	}
	handoff := find("AS last_user_prompt")
	if row := handoff.Rows[0]; row[3] != "feature/digest" || row[8] != "now ship it" || row[9] != "shipped" || row[10] != int64(60) || row[11] != int64(65) {
		t.Fatalf("handoff = %v, want branch feature/digest, last prompt, final reply, ids 60 to 65", row)
	}

	dateBounded := eventIDs(find("AND ev.time_ms >="))
	slices.Sort(dateBounded)
	if !slices.Equal(dateBounded, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("date-bounded recipe = %v, want the five events inside the window and none of the 30-day-old ones", dateBounded)
	}

	resume1 := find("AND EXISTS (SELECT 1 FROM events_settled")
	if len(resume1.Rows) != 1 || resume1.Rows[0][0] != "claude-code:dev1:s1" {
		t.Fatalf("resume step 1 = %v, want the settled session claude-code:dev1:s1 over the newer live one", resume1.Rows)
	}

	resume2 := eventIDs(find("ORDER BY ev.time_ms DESC, ev.id DESC LIMIT 50"))
	if !slices.Equal(resume2, []int64{1, 2, 3}) {
		t.Fatalf("resume step 2 = %v, want s1's three records oldest-first", resume2)
	}

	contextAround := eventIDs(find("e.id BETWEEN 2-5 AND 2+5"))
	if !slices.Equal(contextAround, []int64{1, 2, 3}) {
		t.Fatalf("context-around-hit recipe = %v, want s1's surrounding records 1..3", contextAround)
	}

	fts := find("ORDER BY events_fts.rank LIMIT 10;")
	if ids := eventIDs(fts); !slices.Equal(ids, []int64{2}) {
		t.Fatalf("full-text recipe = %v, want only the event seeded with both terms", ids)
	}
	if sid, ok := fts.Rows[0][1].(string); !ok || sid != "claude-code:dev1:s1" {
		t.Fatalf("full-text session_id = %v, want claude-code:dev1:s1", fts.Rows[0][1])
	}
	if agent, ok := fts.Rows[0][2].(string); !ok || agent != "claude-code" {
		t.Fatalf("full-text agent = %v, want claude-code", fts.Rows[0][2])
	}
	if utc, ok := fts.Rows[0][4].(string); !ok || len(utc) != len("2006-01-02 15:04:05") || utc[4] != '-' || utc[13] != ':' {
		t.Fatalf("full-text utc_time = %v, want formatted UTC timestamp", fts.Rows[0][4])
	}
	if excerpt, ok := fts.Rows[0][5].(string); !ok || !strings.Contains(excerpt, "[authentication]") {
		t.Fatalf("full-text snippet = %v, want highlighted match", fts.Rows[0][5])
	}

	ftsPerSession := find("GROUP BY session_id ORDER BY best_rank")
	if ids := eventIDs(ftsPerSession); !slices.Equal(ids, []int64{2}) {
		t.Fatalf("one-hit-per-session FTS recipe = %v, want event 2", ids)
	}
	if excerpt, ok := ftsPerSession.Rows[0][5].(string); !ok || !strings.Contains(excerpt, "[authentication]") {
		t.Fatalf("one-hit-per-session FTS snippet = %v, want highlighted match", ftsPerSession.Rows[0][5])
	}

	ftsCount := find("count(DISTINCT e.session_id)")
	if len(ftsCount.Rows) != 1 || ftsCount.Rows[0][0] != int64(1) || ftsCount.Rows[0][1] != int64(1) {
		t.Fatalf("count recipe = %v, want 1 session and 1 event", ftsCount.Rows)
	}

	byAgentWindow := find("s.agent = 'claude-code'")
	var agentWindowIDs []string
	for _, row := range byAgentWindow.Rows {
		agentWindowIDs = append(agentWindowIDs, row[0].(string))
	}
	slices.Sort(agentWindowIDs)
	if !slices.Equal(agentWindowIDs, []string{"claude-code:dev1:s-live", "claude-code:dev1:s1", "claude-code:dev1:s2", "claude-code:laptop:s-lap"}) {
		t.Fatalf("agent-window recipe = %v, want the four recent claude-code sessions", agentWindowIDs)
	}

	var modelSessions []string
	for _, row := range find("s.model = 'claude-sonnet-4-5'").Rows {
		modelSessions = append(modelSessions, row[0].(string))
	}
	slices.Sort(modelSessions)
	if !slices.Equal(modelSessions, []string{"claude-code:dev1:s1", "claude-code:laptop:s-lap"}) {
		t.Fatalf("model recipe = %v, want exactly the two seeded claude-sonnet-4-5 sessions", modelSessions)
	}

	device := find("s.device_id = 'laptop'")
	if len(device.Rows) != 1 || device.Rows[0][0] != "claude-code:laptop:s-lap" {
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

	staleSources := find("src.updated_at_ms <")
	if len(staleSources.Rows) != 1 || staleSources.Rows[0][0] != "src-3" {
		t.Fatalf("stale-sources recipe = %v, want only the 40-day-idle src-3", staleSources.Rows)
	}

	biggestSources := find("ORDER BY src.cursor_bytes DESC")
	if len(biggestSources.Rows) < 1 || biggestSources.Rows[0][0] != "src-3" || biggestSources.Rows[0][4] != int64(9000) {
		t.Fatalf("largest-sources recipe = %v, want src-3 with cursor 9000 first", biggestSources.Rows)
	}

	oversized := eventIDs(find("length(e.raw_json) > 65536"))
	if !slices.Equal(oversized, []int64{50}) {
		t.Fatalf("oversized recipe = %v, want only the seeded 70 KiB event 50", oversized)
	}

	noRepo := find("w.repo IS NULL")
	if len(noRepo.Rows) != 1 || noRepo.Rows[0][0] != "claude-code:dev1:s-gone" {
		t.Fatalf("no-repository recipe = %v, want only the seeded gone session", noRepo.Rows)
	}

	emptyIDs := []string{}
	for _, row := range find("ohya-synthetic:").Rows {
		emptyIDs = append(emptyIDs, row[0].(string))
	}
	slices.Sort(emptyIDs)
	if !slices.Equal(emptyIDs, []string{"claude-code:dev1:s-gone", "claude-code:dev1:s-live", "claude-code:laptop:s-lap"}) {
		t.Fatalf("empty-sessions recipe = %v, want the three sessions holding at most one event", emptyIDs)
	}

	replays := find("AS replay_copies")
	if len(replays.Rows) != 1 || replays.Rows[0][0] != int64(1) {
		t.Fatalf("replay-copies recipe = %v, want exactly the one seeded replay copy of s2", replays.Rows)
	}

	models := find("GROUP BY s.model")
	if len(models.Rows) != 2 || models.Rows[0][0] != "claude-3-opus-20240229" || models.Rows[1][0] != "claude-sonnet-4-5" {
		t.Fatalf("model-distribution recipe = %v, want the stale model (oldest activity) first and the current model second", models.Rows)
	}

	marked := find("a.key = 'garbage'")
	if len(marked.Rows) != 1 || marked.Rows[0][0] != "claude-code:dev1:s-old" || marked.Rows[0][3] != "dev1" {
		t.Fatalf("marked-sessions recipe = %v, want s-old marked by dev1", marked.Rows)
	}
}

func TestOneHitPerSessionFTSDeduplicatesSession(t *testing.T) {
	hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	settled := time.Now().UnixMilli() - 3600_000
	seed := []string{
		fmt.Sprintf(`INSERT INTO sessions(id,agent,device_id,native_id,cwd,last_event_at_ms,event_count) VALUES
		 ('claude-code:dev1:s1','claude-code','dev1','s1','/a',%d,3),
		 ('codex:dev1:s2','codex','dev1','s2','/b',%d,1)`, settled, settled),
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-1','dev1','claude-code','/a.jsonl'),('src-2','dev1','codex','/b.jsonl')`,
		fmt.Sprintf(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json) VALUES
		 (1,'claude-code:dev1:s1','src-1',10,%d,'message','assistant','authentication refresh','{}'),
		 (2,'claude-code:dev1:s1','src-1',20,%d,'message','assistant','authentication refresh authentication refresh','{}'),
		 (3,'claude-code:dev1:s1','src-1',30,%d,'message','assistant','long filler words before authentication and later refresh token notes','{}'),
		 (4,'codex:dev1:s2','src-2',10,%d,'message','assistant','authentication refresh in codex','{}')`, settled, settled+10, settled+20, settled+30),
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
	e := NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1"})

	var recipe string
	for _, block := range publishedSQLBlocks(t, GuideAnalyticsReviewURI, AnalyticsReviewGuide(EngineOptions{WriterID: "dev1"}), analyticsGuideSQLBlockCount) {
		if strings.Contains(block, "GROUP BY session_id ORDER BY best_rank") {
			recipe = block
			break
		}
	}
	if recipe == "" {
		t.Fatal("missing one-hit-per-session FTS recipe")
	}
	res := mustQuery(t, e, recipe)
	if res.RowCount != 2 {
		t.Fatalf("deduped rows = %d (%v), want exactly 1 per session (2 total)", res.RowCount, res.Rows)
	}
	seen := map[string]bool{}
	for _, row := range res.Rows {
		sid, _ := row[1].(string)
		seen[sid] = true
		if excerpt, _ := row[5].(string); !strings.Contains(excerpt, "[authentication]") {
			t.Fatalf("row %v missing highlighted snippet", row)
		}
	}
	if !seen["claude-code:dev1:s1"] || !seen["codex:dev1:s2"] {
		t.Fatalf("sessions = %v, want both s1 and s2", seen)
	}
}
