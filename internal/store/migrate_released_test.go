package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/nibzard/ohya/internal/claudecode"
)

type releasedSchema struct {
	release string
	schema  int
}

var releasedSchemas = []releasedSchema{
	{"v0.1.0", 6},
	{"v0.1.1", 6},
	{"v0.1.2", 7},
	{"v0.2.0", 7},
	{"v0.3.0", 8},
}

const releasedSeedMs = 1731800000000

var (
	releasedClaudeSession = SessionID(deviceHex, "claude-code", "rel-sess")
	releasedCodexSession  = SessionID(deviceHex, "codex", "rel-thread")
	releasedClaudeRaw     = `{"type":"user","sessionId":"rel-sess","message":{"role":"user","content":"ohyarelupgrade marker"}}`
	releasedCodexRaw      = `{"type":"response_item","payload":{"type":"message","role":"assistant"}}`
	releasedClaudePath    = "/released/main-session.jsonl"
)

func releasedClaudeCursor() int64 { return int64(len(releasedClaudeRaw)) + 1 }

func releasedCodexCursor() int64 { return 2 * (int64(len(releasedCodexRaw)) + 1) }

func releasedMigrations(t *testing.T, schema int) fs.FS {
	t.Helper()
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := fstest.MapFS{}
	included := 0
	for _, file := range files {
		parts := migrationNameRE.FindStringSubmatch(path.Base(file))
		if parts == nil {
			t.Fatalf("embedded migration %q does not match NNN_name.sql", file)
		}
		version, err := strconv.Atoi(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		if version > schema {
			continue
		}
		data, err := fs.ReadFile(migrationsFS, file)
		if err != nil {
			t.Fatal(err)
		}
		m[file] = &fstest.MapFile{Data: data}
		included = max(included, version)
	}
	if included != schema {
		t.Fatalf("released schema %d does not match the newest embedded migration at or below it (%d)", schema, included)
	}
	return m
}

func seedReleasedDatabase(t *testing.T, db *sql.DB, schema int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	must := func(q string, args ...any) {
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed released database: %v", err)
		}
	}
	must(`INSERT INTO sessions(id,agent,device_id,native_id,cwd,branch,first_prompt,event_count)
		VALUES(?, 'claude-code', ?, 'rel-sess', '/home/agent/released', 'main', 'released first prompt', 1)`,
		releasedClaudeSession, deviceHex)
	must(`INSERT INTO sessions(id,agent,device_id,native_id,event_count)
		VALUES(?, 'codex', ?, 'rel-thread', 1)`, releasedCodexSession, deviceHex)
	must(`INSERT INTO sources(id,device_id,agent,path,cursor_bytes,updated_at_ms)
		VALUES('relsrc-a', ?, 'claude-code', ?, ?, ?)`, deviceHex, releasedClaudePath, releasedClaudeCursor(), releasedSeedMs)
	must(`INSERT INTO sources(id,device_id,agent,path,cursor_bytes,updated_at_ms)
		VALUES('relsrc-b', ?, 'codex', '/released/rollout.jsonl', ?, ?)`, deviceHex, releasedCodexCursor(), releasedSeedMs)
	res, err := tx.Exec(`INSERT INTO events(session_id,source_id,source_offset,kind,role,text,raw_json,parser_version)
		VALUES(?, 'relsrc-a', 0, 'user', 'user', 'released schema fixture ohyarelupgrade', ?, 1)`,
		releasedClaudeSession, releasedClaudeRaw)
	if err != nil {
		t.Fatal(err)
	}
	claudeEventID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	res, err = tx.Exec(`INSERT INTO events(session_id,source_id,source_offset,kind,role,text,raw_json,ordinal,parser_version)
		VALUES(?, 'relsrc-b', 0, 'response_item', 'assistant', 'codex released original', ?, 1, 1)`,
		releasedCodexSession, releasedCodexRaw)
	if err != nil {
		t.Fatal(err)
	}
	originalID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	must(`INSERT INTO events(session_id,source_id,source_offset,kind,role,raw_json,ordinal,duplicate_of,parser_version)
		VALUES(?, 'relsrc-b', ?, 'response_item', 'assistant', ?, 1, ?, 1)`,
		releasedCodexSession, int64(len(releasedCodexRaw))+1, releasedCodexRaw, originalID)
	must(`INSERT INTO session_token_usage(session_id,message_id,input_tokens,cache_creation_input_tokens,cache_read_input_tokens,output_tokens)
		VALUES(?, 'msg_rel_1', 11, 7, 5, 23)`, releasedClaudeSession)
	must(`INSERT INTO event_annotations(event_id,namespace,key,value,writer_id,created_at_ms,updated_at_ms)
		VALUES(?, 'tags', 'released', 'true', ?, ?, ?)`, claudeEventID, deviceHex, releasedSeedMs, releasedSeedMs)
	if schema >= 7 {
		must(`INSERT INTO workspaces(device_id,path,repo,observed_at_ms)
			VALUES(?, '/home/agent/released', 'github.com/owner/released', ?)`, deviceHex, releasedSeedMs)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func buildReleasedDatabase(t *testing.T, schema int) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "released.db")
	db, err := sql.Open("sqlite3", writerDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), db, releasedMigrations(t, schema)); err != nil {
		db.Close()
		t.Fatalf("build a database at released schema %d: %v", schema, err)
	}
	seedReleasedDatabase(t, db, schema)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func assertReleasedRowsSurviveUpgrade(t *testing.T, db *sql.DB, fromSchema, toSchema int) {
	t.Helper()
	var claudeEventID int
	var raw string
	if err := db.QueryRow(`SELECT id, raw_json FROM events WHERE source_id='relsrc-a' AND source_offset=0`).
		Scan(&claudeEventID, &raw); err != nil {
		t.Fatal(err)
	}
	if raw != releasedClaudeRaw {
		t.Fatalf("raw_json of the released event changed: %q", raw)
	}
	var firstPrompt string
	if err := db.QueryRow(`SELECT first_prompt FROM sessions WHERE id=?`, releasedClaudeSession).
		Scan(&firstPrompt); err != nil || firstPrompt != "released first prompt" {
		t.Fatalf("first_prompt after upgrade = %q, %v", firstPrompt, err)
	}
	var originalID int
	if err := db.QueryRow(`SELECT id FROM events WHERE source_id='relsrc-b' AND source_offset=0`).
		Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	var duplicateOf int
	var duplicateText sql.NullString
	if err := db.QueryRow(`SELECT duplicate_of, text FROM events WHERE source_id='relsrc-b' AND source_offset=?`,
		int64(len(releasedCodexRaw))+1).Scan(&duplicateOf, &duplicateText); err != nil {
		t.Fatal(err)
	}
	if duplicateOf != originalID || duplicateText.Valid {
		t.Fatalf("replay duplicate after upgrade: duplicate_of=%d (original %d), text=%v",
			duplicateOf, originalID, duplicateText)
	}
	var input, output int
	if err := db.QueryRow(`SELECT input_tokens, output_tokens FROM session_token_usage
		WHERE session_id=? AND message_id='msg_rel_1'`, releasedClaudeSession).Scan(&input, &output); err != nil || input != 11 || output != 23 {
		t.Fatalf("token usage after upgrade = %d/%d, %v", input, output, err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM event_annotations WHERE event_id=?`, claudeEventID).
		Scan(&value); err != nil || value != "true" {
		t.Fatalf("annotation after upgrade = %q, %v", value, err)
	}
	var ftsID int
	if err := db.QueryRow(`SELECT rowid FROM events_fts WHERE events_fts MATCH 'ohyarelupgrade'`).
		Scan(&ftsID); err != nil || ftsID != claudeEventID {
		t.Fatalf("FTS lookup after upgrade matched event %d, want the released event %d: %v", ftsID, claudeEventID, err)
	}
	var settled int
	if err := db.QueryRow(`SELECT count(*) FROM events_settled WHERE session_id=?`, releasedClaudeSession).
		Scan(&settled); err != nil || settled != 1 {
		t.Fatalf("events_settled rows for the released session = %d, %v", settled, err)
	}
	if toSchema >= 7 {
		if !tableExists(t, db, "workspaces") {
			t.Fatal("workspaces table missing after upgrade")
		}
		if fromSchema >= 7 {
			var repo string
			if err := db.QueryRow(`SELECT repo FROM workspaces WHERE device_id=? AND path='/home/agent/released'`,
				deviceHex).Scan(&repo); err != nil || repo != "github.com/owner/released" {
				t.Fatalf("released workspaces row after upgrade = %q, %v", repo, err)
			}
		}
	}
	if !tableExists(t, db, "purged_sources") {
		t.Fatal("purged_sources table missing after upgrade")
	}
}

func ingestAfterReleasedUpgrade(t *testing.T, hub *DB) {
	t.Helper()
	ctx := context.Background()
	if err := hub.RegisterSource(ctx, "relsrc-a", deviceHex, "claude-code", releasedClaudePath); err != nil {
		t.Fatalf("re-register the released source after upgrade: %v", err)
	}
	var cursor int64
	if err := hub.Writer().QueryRow(`SELECT cursor_bytes FROM sources WHERE id='relsrc-a'`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != releasedClaudeCursor() {
		t.Fatalf("released cursor after upgrade = %d, want %d", cursor, releasedClaudeCursor())
	}
	raw := `{"type":"user","sessionId":"rel-sess","message":{"role":"user","content":"after the released upgrade ohyarelpost"}}`
	res := mustIngest(t, hub, Batch{
		SourceID: "relsrc-a",
		Events: []Event{{
			Offset:        cursor,
			End:           cursor + int64(len(raw)) + 1,
			NativeID:      "rel-sess",
			Kind:          "user",
			Role:          "user",
			Text:          "after the released upgrade ohyarelpost",
			RawJSON:       raw,
			ParserVersion: claudecode.OutputVersion,
		}},
	})
	wantCursor := cursor + int64(len(raw)) + 1
	if res.Inserted != 1 || res.Cursor != wantCursor {
		t.Fatalf("ingest after upgrade: inserted %d, cursor %d; want 1 and %d", res.Inserted, res.Cursor, wantCursor)
	}
	var eventCount int
	if err := hub.Writer().QueryRow(`SELECT event_count FROM sessions WHERE id=?`, releasedClaudeSession).
		Scan(&eventCount); err != nil || eventCount != 2 {
		t.Fatalf("event_count after post-upgrade ingest = %d, %v", eventCount, err)
	}
	var indexed int
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH 'ohyarelpost'`).
		Scan(&indexed); err != nil || indexed != 1 {
		t.Fatalf("post-upgrade ingest is missing from the FTS index: %d, %v", indexed, err)
	}
}

func TestMigrateUpgradesReleasedSchemas(t *testing.T) {
	current, err := SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range releasedSchemas {
		t.Run(rel.release, func(t *testing.T) {
			dbPath := buildReleasedDatabase(t, rel.schema)
			hub, err := Open(context.Background(), dbPath)
			if err != nil {
				t.Fatalf("upgrade a %s database (schema %d) through store.Open: %v", rel.release, rel.schema, err)
			}
			t.Cleanup(func() { hub.Close() })
			db := hub.Writer()
			var applied int
			if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&applied); err != nil {
				t.Fatal(err)
			}
			if applied != current {
				t.Fatalf("schema after upgrading from %s = %d, want %d", rel.release, applied, current)
			}
			assertReleasedRowsSurviveUpgrade(t, db, rel.schema, current)
			ingestAfterReleasedUpgrade(t, hub)
		})
	}
}

func TestReleasedSchemasMatchPublishedTags(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable; published tags cannot be verified")
	}
	root := filepath.Join("..", "..")
	out, err := exec.Command(git, "-C", root, "tag", "-l", "v[0-9]*").Output()
	if err != nil {
		t.Skipf("cannot list published tags: %v", err)
	}
	tags := strings.Fields(string(out))
	if len(tags) == 0 {
		t.Skip("no published tags are visible in this checkout")
	}
	byRelease := map[string]int{}
	for _, rel := range releasedSchemas {
		byRelease[rel.release] = rel.schema
	}
	for _, tag := range tags {
		schema, ok := byRelease[tag]
		if !ok {
			t.Errorf("published release %s is missing from releasedSchemas; the change that publishes a release adds it there and in DB.md's upgrade policy", tag)
			continue
		}
		assertTagMatchesSchema(t, git, root, tag, schema)
	}
}

func assertTagMatchesSchema(t *testing.T, git, root, tag string, schema int) {
	t.Helper()
	out, err := exec.Command(git, "-C", root, "ls-tree", "--name-only", tag, "internal/store/migrations/").Output()
	if err != nil {
		t.Skipf("cannot list the migrations of %s: %v", tag, err)
	}
	shipped := 0
	for _, line := range strings.Fields(string(out)) {
		base := path.Base(line)
		if migrationNameRE.FindStringSubmatch(base) == nil {
			continue
		}
		shipped++
		embedded, err := fs.ReadFile(migrationsFS, "migrations/"+base)
		if err != nil {
			t.Errorf("release %s ships %s, which this tree does not embed: %v", tag, base, err)
			continue
		}
		blob, err := exec.Command(git, "-C", root, "show", tag+":"+line).Output()
		if err != nil {
			t.Skipf("cannot read %s of %s: %v", line, tag, err)
		}
		if string(blob) != string(embedded) {
			t.Errorf("migration %s differs between %s and this tree; applied migrations are immutable", base, tag)
		}
	}
	if shipped != schema {
		t.Errorf("release %s ships %d migrations; releasedSchemas says %d", tag, shipped, schema)
	}
}
