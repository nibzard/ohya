package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// annotateRow reads one annotation row, failing the test when it is absent.
func annotateRow(t *testing.T, db *sql.DB, event int64, namespace, key, writerID string) (value string, created, updated int64) {
	t.Helper()
	err := db.QueryRow(
		`SELECT value, created_at_ms, updated_at_ms FROM event_annotations
		 WHERE event_id = ? AND namespace = ? AND key = ? AND writer_id = ?`,
		event, namespace, key, writerID).Scan(&value, &created, &updated)
	if err != nil {
		t.Fatalf("read annotation (%d, %s, %s, %s): %v", event, namespace, key, writerID, err)
	}
	return value, created, updated
}

// annotationCount counts annotation rows matching the where fragment.
func annotationCount(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM event_annotations WHERE `+where, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAnnotateUpsertLifecycle(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	upsert := func(value string) AnnotateResult {
		t.Helper()
		result, err := Annotate(ctx, db, "dev1", []AnnotationOp{
			{Op: AnnotationUpsert, EventID: 1, Namespace: "tags", Key: "needs-review", Value: value},
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", value, err)
		}
		return result
	}

	// Insert: one row, creation and update set together.
	if got := upsert(`{"status":"open"}`); got != (AnnotateResult{Applied: 1}) {
		t.Fatalf("first upsert = %+v", got)
	}
	value, created, updated := annotateRow(t, db, 1, "tags", "needs-review", "dev1")
	if value != `{"status":"open"}` || created != updated {
		t.Fatalf("row = (%q, %d, %d)", value, created, updated)
	}

	// Identical retry: a full no-op, timestamps included. Move both stamps
	// back so a rewrite in the same millisecond cannot fake the result.
	if _, err := db.Exec(
		`UPDATE event_annotations SET created_at_ms = created_at_ms - 60000, updated_at_ms = updated_at_ms - 60000`); err != nil {
		t.Fatal(err)
	}
	_, created, updated = annotateRow(t, db, 1, "tags", "needs-review", "dev1")
	if got := upsert(`{"status":"open"}`); got != (AnnotateResult{Unchanged: 1}) {
		t.Fatalf("identical retry = %+v", got)
	}
	if _, c, u := annotateRow(t, db, 1, "tags", "needs-review", "dev1"); c != created || u != updated {
		t.Fatalf("identical retry rewrote timestamps: (%d, %d), want (%d, %d)", c, u, created, updated)
	}

	// Changed value: complete replacement, creation preserved, update bumped.
	if got := upsert(`{"status":"done"}`); got != (AnnotateResult{Applied: 1}) {
		t.Fatalf("changed upsert = %+v", got)
	}
	if v, c, u := annotateRow(t, db, 1, "tags", "needs-review", "dev1"); v != `{"status":"done"}` ||
		c != created || u <= updated {
		t.Fatalf("changed row = (%q, %d, %d), want value replaced, created %d kept, update past %d",
			v, c, u, created, updated)
	}

	// Delete, then delete of the absent annotation.
	result, err := Annotate(ctx, db, "dev1", []AnnotationOp{
		{Op: AnnotationDelete, EventID: 1, Namespace: "tags", Key: "needs-review"},
	})
	if err != nil || result != (AnnotateResult{Applied: 1}) {
		t.Fatalf("delete = %+v, %v", result, err)
	}
	result, err = Annotate(ctx, db, "dev1", []AnnotationOp{
		{Op: AnnotationDelete, EventID: 1, Namespace: "tags", Key: "needs-review"},
	})
	if err != nil || result != (AnnotateResult{Unchanged: 1}) {
		t.Fatalf("delete of absent annotation = %+v, %v; it must succeed as unchanged", result, err)
	}
	if got := annotationCount(t, db, "1=1"); got != 0 {
		t.Fatalf("annotations after delete = %d", got)
	}

	// An empty batch is accepted and commits nothing.
	if result, err := Annotate(ctx, db, "dev1", nil); err != nil || result != (AnnotateResult{}) {
		t.Fatalf("empty batch = %+v, %v", result, err)
	}
}

func TestAnnotateBatchIsAtomic(t *testing.T) {
	db := setup(t)
	ctx := context.Background()

	// The valid operations surround the missing-event operation; none of the
	// batch may survive it.
	_, err := Annotate(ctx, db, "dev1", []AnnotationOp{
		{Op: AnnotationUpsert, EventID: 1, Namespace: "tags", Key: "a", Value: "1"},
		{Op: AnnotationUpsert, EventID: 42, Namespace: "tags", Key: "b", Value: "2"},
		{Op: AnnotationUpsert, EventID: 1, Namespace: "tags", Key: "c", Value: "3"},
	})
	if err == nil || !strings.Contains(err.Error(), "42") {
		t.Fatalf("missing event error = %v, want event id 42 named", err)
	}
	if got := annotationCount(t, db, "1=1"); got != 0 {
		t.Fatalf("partial batch committed %d rows", got)
	}

	// A duplicate identity rejects the batch before any statement runs.
	_, err = Annotate(ctx, db, "dev1", []AnnotationOp{
		{Op: AnnotationUpsert, EventID: 1, Namespace: "tags", Key: "a", Value: "1"},
		{Op: AnnotationDelete, EventID: 1, Namespace: "tags", Key: "a"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate identity") {
		t.Fatalf("duplicate identity error = %v", err)
	}
	if got := annotationCount(t, db, "1=1"); got != 0 {
		t.Fatalf("duplicate batch committed %d rows", got)
	}

	// One valid batch of mixed operations reports both counts.
	result, err := Annotate(ctx, db, "dev1", []AnnotationOp{
		{Op: AnnotationUpsert, EventID: 1, Namespace: "tags", Key: "a", Value: `[1,2]`},
		{Op: AnnotationUpsert, EventID: 1, Namespace: "tags", Key: "b", Value: `{"ok":true}`},
		{Op: AnnotationDelete, EventID: 1, Namespace: "tags", Key: "absent"},
	})
	if err != nil || result != (AnnotateResult{Applied: 2, Unchanged: 1}) {
		t.Fatalf("mixed batch = %+v, %v", result, err)
	}
}

func TestAnnotateRejectsInvalidBatches(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	tooLong := strings.Repeat("a", 129)
	bigValue := `"` + strings.Repeat("a", MaxAnnotationValueBytes) + `"` // one byte over once quoted

	cases := []struct {
		name string
		ops  []AnnotationOp
		want string
	}{
		{"unknown op", []AnnotationOp{{Op: "merge", EventID: 1, Namespace: "n", Key: "k", Value: "1"}}, `unknown op "merge"`},
		{"zero event id", []AnnotationOp{{Op: AnnotationUpsert, EventID: 0, Namespace: "n", Key: "k", Value: "1"}}, "positive integer"},
		{"negative event id", []AnnotationOp{{Op: AnnotationDelete, EventID: -3, Namespace: "n", Key: "k"}}, "positive integer"},
		{"empty namespace", []AnnotationOp{{Op: AnnotationDelete, EventID: 1, Namespace: "", Key: "k"}}, "namespace"},
		{"long namespace", []AnnotationOp{{Op: AnnotationDelete, EventID: 1, Namespace: tooLong, Key: "k"}}, "namespace"},
		{"non-ascii key", []AnnotationOp{{Op: AnnotationDelete, EventID: 1, Namespace: "n", Key: "schlüssel"}}, "key"},
		{"upsert without value", []AnnotationOp{{Op: AnnotationUpsert, EventID: 1, Namespace: "n", Key: "k"}}, "requires a JSON value"},
		{"upsert with invalid JSON", []AnnotationOp{{Op: AnnotationUpsert, EventID: 1, Namespace: "n", Key: "k", Value: "{nope"}}, "not valid JSON"},
		{"oversized value", []AnnotationOp{{Op: AnnotationUpsert, EventID: 1, Namespace: "n", Key: "k", Value: bigValue}}, "byte limit"},
		{"delete with value", []AnnotationOp{{Op: AnnotationDelete, EventID: 1, Namespace: "n", Key: "k", Value: "1"}}, "delete takes no value"},
	}
	for _, tc := range cases {
		_, err := Annotate(ctx, db, "dev1", tc.ops)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}

	// Batch too large: one over the limit fails, exactly at the limit works.
	var ops []AnnotationOp
	for i := 0; i <= MaxAnnotationOperations; i++ {
		ops = append(ops, AnnotationOp{Op: AnnotationDelete, EventID: 1, Namespace: "n", Key: fmt.Sprint(i)})
	}
	if _, err := Annotate(ctx, db, "dev1", ops); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized batch error = %v", err)
	}
	ops = ops[:MaxAnnotationOperations]
	if result, err := Annotate(ctx, db, "dev1", ops); err != nil || result.Unchanged != MaxAnnotationOperations {
		t.Fatalf("batch at the limit = %+v, %v", result, err)
	}

	// No writer identity: the hub cannot accept one from caller input.
	if _, err := Annotate(ctx, db, "", []AnnotationOp{
		{Op: AnnotationDelete, EventID: 1, Namespace: "n", Key: "k"},
	}); err == nil || !strings.Contains(err.Error(), "writer ID") {
		t.Fatalf("empty writer error = %v", err)
	}

	// A value SQLite itself refuses to validate — its JSON depth limit is
	// lower than Go's — rejects the whole batch through the table check,
	// atomically, with nothing applied.
	deep := strings.Repeat("[", 1100) + strings.Repeat("]", 1100)
	_, err := Annotate(ctx, db, "dev1", []AnnotationOp{
		{Op: AnnotationUpsert, EventID: 1, Namespace: "tags", Key: "deep", Value: deep},
		{Op: AnnotationUpsert, EventID: 1, Namespace: "tags", Key: "shallow", Value: "1"},
	})
	if err == nil || !strings.Contains(err.Error(), "json_valid") {
		t.Fatalf("deep value error = %v, want the storage check named", err)
	}
	if got := annotationCount(t, db, "1=1"); got != 0 {
		t.Fatalf("batch with a storable-value failure committed %d rows", got)
	}

	// Nothing above wrote anything.
	if got := annotationCount(t, db, "1=1"); got != 0 {
		t.Fatalf("rejected batches committed %d rows", got)
	}
}

func TestAnnotateWriterIsolationAndConcurrentWrites(t *testing.T) {
	db := setup(t)
	ctx := context.Background()

	// Two writers on one (event, namespace, key) hold two independent rows.
	for _, w := range []string{"dev1", "dev2"} {
		result, err := Annotate(ctx, db, w, []AnnotationOp{
			{Op: AnnotationUpsert, EventID: 1, Namespace: "security", Key: "review", Value: `"one"`},
		})
		if err != nil || result.Applied != 1 {
			t.Fatalf("writer %s upsert = %+v, %v", w, result, err)
		}
	}
	if got := annotationCount(t, db, "1=1"); got != 2 {
		t.Fatalf("rows for two writers = %d, want 2", got)
	}

	// A writer deletes only its own annotation; the other row survives.
	result, err := Annotate(ctx, db, "dev2", []AnnotationOp{
		{Op: AnnotationDelete, EventID: 1, Namespace: "security", Key: "review"},
	})
	if err != nil || result != (AnnotateResult{Applied: 1}) {
		t.Fatalf("dev2 delete = %+v, %v", result, err)
	}
	if value, _, _ := annotateRow(t, db, 1, "security", "review", "dev1"); value != `"one"` {
		t.Fatalf("dev1 row after dev2 delete = %q", value)
	}

	// Concurrent upserts to one identity serialize on the single writer; the
	// last committed value wins and exactly one row exists.
	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Annotate(ctx, db, "dev1", []AnnotationOp{
				{Op: AnnotationUpsert, EventID: 1, Namespace: "race", Key: "k", Value: fmt.Sprint(i)},
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent upsert %d: %v", i, err)
		}
	}
	value, _, _ := annotateRow(t, db, 1, "race", "k", "dev1")
	want := make([]string, writers)
	for i := range want {
		want[i] = fmt.Sprint(i)
	}
	if !slices.Contains(want, value) {
		t.Fatalf("last committed value = %q, want one of %v", value, want)
	}
	if got := annotationCount(t, db, "namespace = 'race'"); got != 1 {
		t.Fatalf("race rows = %d, want 1", got)
	}
}

func TestAnnotateLeavesSourceDataUntouched(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	if _, err := db.Exec(
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text)
		 VALUES(2,'s','f',10,'message','{"second":"record"}','second record')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sources SET cursor_bytes = 30 WHERE id = 'f'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO event_annotations VALUES(1,'old','note','1','dev9',5,5)`); err != nil {
		t.Fatal(err)
	}

	// Snapshot before: events, sessions, sources, the other writer's
	// annotation, and the FTS index.
	snapshot := func() (events, sessions, sources, foreign string, fts int) {
		t.Helper()
		row := func(query string) string {
			var s string
			if err := db.QueryRow(query).Scan(&s); err != nil {
				t.Fatalf("snapshot %q: %v", query, err)
			}
			return s
		}
		events = row(`SELECT group_concat(id || ':' || raw_json || ':' || ifnull(text,'')) FROM (SELECT * FROM events ORDER BY id)`)
		sessions = row(`SELECT group_concat(id || ':' || event_count || ':' || ifnull(first_prompt,'')) FROM (SELECT * FROM sessions ORDER BY id)`)
		sources = row(`SELECT id || ':' || cursor_bytes FROM sources`)
		foreign = row(`SELECT event_id || ':' || namespace || ':' || value || ':' || created_at_ms || ':' || updated_at_ms
		 FROM event_annotations WHERE writer_id = 'dev9'`)
		if err := db.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH 'hello'`).Scan(&fts); err != nil {
			t.Fatal(err)
		}
		return
	}
	e1, s1, c1, f1, t1 := snapshot()

	// Annotate hard: upserts, deletes, retries, another writer's identity.
	for _, batch := range [][]AnnotationOp{
		{{Op: AnnotationUpsert, EventID: 2, Namespace: "tags", Key: "keep", Value: "true"}},
		{{Op: AnnotationUpsert, EventID: 2, Namespace: "tags", Key: "keep", Value: "true"}}, // identical retry
		{{Op: AnnotationUpsert, EventID: 2, Namespace: "tags", Key: "keep", Value: "false"}},
		{{Op: AnnotationDelete, EventID: 1, Namespace: "old", Key: "note"}}, // dev9 owns it: absent for dev1
	} {
		if _, err := Annotate(ctx, db, "dev1", batch); err != nil {
			t.Fatalf("batch: %v", err)
		}
	}

	// The FTS index stays consistent with its content table.
	if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity check: %v", err)
	}
	e2, s2, c2, f2, t2 := snapshot()
	if e1 != e2 || s1 != s2 || c1 != c2 || f1 != f2 || t1 != t2 {
		t.Fatalf("annotate changed source data:\nevents %q → %q\nsessions %q → %q\nsources %q → %q\nforeign annotation %q → %q\nfts %d → %d",
			e1, e2, s1, s2, c1, c2, f1, f2, t1, t2)
	}
}
