package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nibzard/ohya/internal/store"
)

// callAnnotate runs one annotate tool call with raw JSON arguments, so tests
// control the exact bytes on the wire.
func callAnnotate(t *testing.T, session *mcp.ClientSession, arguments string) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      AnnotateToolName,
		Arguments: json.RawMessage(arguments),
	})
	if err != nil {
		t.Fatalf("CallTool(annotate, %s): %v", arguments, err)
	}
	return result
}

// annotateCounts decodes a successful annotate result.
func annotateCounts(t *testing.T, result *mcp.CallToolResult) store.AnnotateResult {
	t.Helper()
	if result.IsError {
		t.Fatalf("annotate failed: %s", toolText(result))
	}
	var payload store.AnnotateResult
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("structured content %s: %v", raw, err)
	}
	return payload
}

// toolText renders a tool result's text content.
func toolText(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range result.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// queryRows runs one sql tool call and decodes its rows.
func queryRows(t *testing.T, session *mcp.ClientSession, query string) Result {
	t.Helper()
	var payload Result
	if err := redecode(call(t, session, query), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestAnnotateToolAppliesBatches(t *testing.T) {
	session := connect(t, EngineOptions{WriterID: "dev1"})

	// Upsert plus delete of an absent annotation: one applied, one unchanged.
	result := callAnnotate(t, session, `{"operations":[
		{"op":"upsert","event_id":2,"namespace":"tags","key":"flag","value":{"seen": true}},
		{"op":"delete","event_id":2,"namespace":"tags","key":"absent"}]}`)
	if got := annotateCounts(t, result); got != (store.AnnotateResult{Applied: 1, Unchanged: 1}) {
		t.Fatalf("first batch = %+v", got)
	}

	// The annotation is stored compacted and readable through the sql tool.
	r := queryRows(t, session, `SELECT value FROM event_annotations
		WHERE event_id = 2 AND namespace = 'tags' AND key = 'flag' AND writer_id = 'dev1'`)
	if fmt.Sprint(r.Rows) != `[[{"seen":true}]]` {
		t.Fatalf("stored value = %v, want compacted {\"seen\":true}", r.Rows)
	}

	// An identical retry — same batch, same spaced value — changes nothing.
	result = callAnnotate(t, session, `{"operations":[
		{"op":"upsert","event_id":2,"namespace":"tags","key":"flag","value":{"seen": true}},
		{"op":"delete","event_id":2,"namespace":"tags","key":"absent"}]}`)
	if got := annotateCounts(t, result); got != (store.AnnotateResult{Unchanged: 2}) {
		t.Fatalf("retried batch = %+v", got)
	}

	// A changed value replaces the whole value.
	result = callAnnotate(t, session, `{"operations":[
		{"op":"upsert","event_id":2,"namespace":"tags","key":"flag","value":{"seen":false,"note":"replaced"}}]}`)
	if got := annotateCounts(t, result); got != (store.AnnotateResult{Applied: 1}) {
		t.Fatalf("changed batch = %+v", got)
	}

	// The seeded annotation from another test round is this writer's own: an
	// identical upsert is a no-op.
	result = callAnnotate(t, session, `{"operations":[
		{"op":"upsert","event_id":1,"namespace":"security","key":"review","value":{"status":"reviewed"}}]}`)
	if got := annotateCounts(t, result); got != (store.AnnotateResult{Unchanged: 1}) {
		t.Fatalf("seeded retry = %+v", got)
	}

	// An empty batch is accepted and reports zeros.
	result = callAnnotate(t, session, `{"operations":[]}`)
	if got := annotateCounts(t, result); got != (store.AnnotateResult{}) {
		t.Fatalf("empty batch = %+v", got)
	}

	// Original records stay untouched: the same events and sessions remain,
	// and only one annotation row was added to the seeded one.
	r = queryRows(t, session, `SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM sessions),
		(SELECT count(*) FROM event_annotations)`)
	if fmt.Sprint(r.Rows) != "[[2 1 2]]" {
		t.Fatalf("database after annotate = %v", r.Rows)
	}
}

func TestAnnotateToolDescriptionPublishesContract(t *testing.T) {
	session := connect(t, EngineOptions{WriterID: "dev1"})
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name != AnnotateToolName {
			continue
		}
		description := tl.Description
		for _, want := range []string{
			"dev1", "writer_id", fmt.Sprint(store.MaxAnnotationOperations),
			fmt.Sprint(DefaultMaxAnnotateRequestBytes), fmt.Sprint(store.MaxAnnotationValueBytes),
			"applied", "unchanged", "event_annotations", "created_at_ms", "duplicate",
		} {
			if !strings.Contains(description, want) {
				t.Fatalf("annotate description misses %q:\n%s", want, description)
			}
		}
		return
	}
	t.Fatal("annotate tool not listed")
}

func TestAnnotateToolRejectsInvalidRequests(t *testing.T) {
	session := connect(t, EngineOptions{WriterID: "dev1"})

	cases := []struct {
		name      string
		arguments string
		want      string // substring of the tool error; empty for any error
	}{
		{
			"writer spoofing",
			`{"operations":[{"op":"upsert","event_id":2,"namespace":"tags","key":"a","value":1,"writer_id":"attacker"}]}`,
			"",
		},
		{
			"unknown top-level field",
			`{"operations":[{"op":"delete","event_id":2,"namespace":"tags","key":"a"}],"force":true}`,
			"",
		},
		{"missing op", `{"operations":[{"event_id":2,"namespace":"tags","key":"a","value":1}]}`, ""},
		{"missing event_id", `{"operations":[{"op":"upsert","namespace":"tags","key":"a","value":1}]}`, ""},
		{"missing namespace", `{"operations":[{"op":"upsert","event_id":2,"key":"a","value":1}]}`, ""},
		{"missing key", `{"operations":[{"op":"upsert","event_id":2,"namespace":"tags","value":1}]}`, ""},
		{"unknown op", `{"operations":[{"op":"dlete","event_id":2,"namespace":"tags","key":"a"}]}`, `unknown op "dlete"`},
		{"upsert without value", `{"operations":[{"op":"upsert","event_id":2,"namespace":"tags","key":"a"}]}`, "requires a JSON value"},
		{"delete with value", `{"operations":[{"op":"delete","event_id":2,"namespace":"tags","key":"a","value":null}]}`, "delete takes no value"},
		{"bad namespace", `{"operations":[{"op":"delete","event_id":2,"namespace":"has space","key":"a"}]}`, "namespace"},
		{"empty key", `{"operations":[{"op":"delete","event_id":2,"namespace":"tags","key":""}]}`, "key"},
		{"null operations", `{"operations":null}`, "missing operations"},
		{"operations not an array", `{"operations":3}`, ""},
		{"operation not an object", `{"operations":[3]}`, ""},
		{
			"duplicate identity",
			`{"operations":[{"op":"upsert","event_id":2,"namespace":"tags","key":"a","value":1},
			                {"op":"delete","event_id":2,"namespace":"tags","key":"a"}]}`,
			"duplicate identity",
		},
		{
			"missing event rejects the whole batch",
			`{"operations":[{"op":"upsert","event_id":2,"namespace":"tags","key":"ok","value":1},
			                {"op":"upsert","event_id":999,"namespace":"tags","key":"missing","value":2}]}`,
			"999",
		},
	}
	for _, tc := range cases {
		result := callAnnotate(t, session, tc.arguments)
		if !result.IsError {
			t.Fatalf("%s: annotate accepted %s", tc.name, tc.arguments)
		}
		if text := toolText(result); tc.want != "" && !strings.Contains(text, tc.want) {
			t.Fatalf("%s: error %q misses %q", tc.name, text, tc.want)
		}
	}

	// One over the operation limit fails.
	var ops []string
	for i := 0; i <= store.MaxAnnotationOperations; i++ {
		ops = append(ops, fmt.Sprintf(`{"op":"delete","event_id":2,"namespace":"tags","key":"k%d"}`, i))
	}
	result := callAnnotate(t, session, `{"operations":[`+strings.Join(ops, ",")+`]}`)
	if !result.IsError || !strings.Contains(toolText(result), "limit") {
		t.Fatalf("oversized batch error = %q", toolText(result))
	}

	// Every rejection above applied nothing.
	r := queryRows(t, session, `SELECT count(*) FROM event_annotations`)
	if fmt.Sprint(r.Rows) != "[[1]]" {
		t.Fatalf("annotations after rejections = %v, want only the seeded row", r.Rows)
	}
}

func TestAnnotateToolRejectsOversizedRequests(t *testing.T) {
	// A small limit plus a valid but long namespace (up to 128 characters):
	// the request passes every schema and field rule, only its size fails.
	session := connect(t, EngineOptions{WriterID: "dev1", MaxAnnotateRequestBytes: 120})
	result := callAnnotate(t, session, `{"operations":[{"op":"delete","event_id":2,"namespace":"`+
		strings.Repeat("n", 100)+`","key":"a"}]}`)
	if !result.IsError || !strings.Contains(toolText(result), "byte limit") {
		t.Fatalf("oversized request error = %q", toolText(result))
	}
	// The tool stays usable afterwards.
	result = callAnnotate(t, session, `{"operations":[]}`)
	if got := annotateCounts(t, result); got != (store.AnnotateResult{}) {
		t.Fatalf("after rejection = %+v", got)
	}
}

func TestAnnotateWriterIsolation(t *testing.T) {
	hub, readers := openSeededHub(t)
	local := NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1"})
	remote := NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev2"})
	ctx := context.Background()

	// Both engines annotate the same (event, namespace, key) independently.
	op := `{"operations":[{"op":"upsert","event_id":2,"namespace":"security","key":"review","value":"mine"}]}`
	for name, e := range map[string]*Engine{"dev1": local, "dev2": remote} {
		result, err := e.Annotate(ctx, json.RawMessage(op))
		if err != nil || result.Applied != 1 {
			t.Fatalf("%s upsert = %+v, %v", name, result, err)
		}
	}
	var rows int
	if err := hub.Writer().QueryRow(
		`SELECT count(*) FROM event_annotations WHERE event_id = 2 AND namespace = 'security'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows for two writers = %d, want 2", rows)
	}

	// Each engine sees and changes only its own annotation.
	if result, err := remote.Annotate(ctx, json.RawMessage(
		`{"operations":[{"op":"delete","event_id":2,"namespace":"security","key":"review"}]}`)); err != nil ||
		result != (store.AnnotateResult{Applied: 1}) {
		t.Fatalf("dev2 delete = %+v, %v", result, err)
	}
	var dev1Value string
	if err := hub.Writer().QueryRow(
		`SELECT value FROM event_annotations WHERE event_id = 2 AND writer_id = 'dev1'`).Scan(&dev1Value); err != nil || dev1Value != `"mine"` {
		t.Fatalf("dev1 row after dev2 delete = %q, %v", dev1Value, err)
	}

	// A caller cannot choose the writer identity: writer_id is not a field of
	// the wire shape, so supplying it fails the strict decode.
	_, err := local.Annotate(ctx, json.RawMessage(
		`{"operations":[{"op":"upsert","event_id":2,"namespace":"tags","key":"spoof","value":1,"writer_id":"attacker"}]}`))
	if err == nil || !strings.Contains(err.Error(), "writer_id") {
		t.Fatalf("spoofed writer error = %v, want the unknown field named", err)
	}
	var spoofed int
	if err := hub.Writer().QueryRow(
		`SELECT count(*) FROM event_annotations WHERE key = 'spoof'`).Scan(&spoofed); err != nil || spoofed != 0 {
		t.Fatalf("spoofed rows = %d, want 0", spoofed)
	}
}

func TestAnnotateConcurrentToolCallsTakeLastCommit(t *testing.T) {
	session := connect(t, EngineOptions{WriterID: "dev1"})
	const calls = 4
	results := make([]*mcp.CallToolResult, calls)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = callAnnotate(t, session, fmt.Sprintf(
				`{"operations":[{"op":"upsert","event_id":2,"namespace":"race","key":"k","value":%d}]}`, i))
		}(i)
	}
	wg.Wait()
	for i, result := range results {
		if result.IsError {
			t.Fatalf("concurrent call %d failed: %s", i, toolText(result))
		}
	}
	r := queryRows(t, session, `SELECT value, count(*) OVER () FROM event_annotations
		WHERE event_id = 2 AND namespace = 'race' AND writer_id = 'dev1'`)
	if len(r.Rows) != 1 {
		t.Fatalf("race rows = %v, want exactly one", r.Rows)
	}
}

// TestAnnotateQueryToolSeesAnnotations closes the loop: what annotate
// commits is immediately visible to the read-only sql tool on a reader
// connection, not only on the writer.
func TestAnnotateQueryToolSeesAnnotations(t *testing.T) {
	session := connect(t, EngineOptions{WriterID: "dev1"})
	result := callAnnotate(t, session, `{"operations":[
		{"op":"upsert","event_id":2,"namespace":"classification","key":"work-type","value":{"label":"refactor"}}]}`)
	if got := annotateCounts(t, result); got.Applied != 1 {
		t.Fatalf("upsert = %+v", got)
	}
	r := queryRows(t, session, `SELECT a.namespace, a.key, json_extract(a.value, '$.label')
		FROM event_annotations a WHERE a.event_id = 2`)
	if fmt.Sprint(r.Rows) != `[[classification work-type refactor]]` {
		t.Fatalf("rows = %v", r.Rows)
	}
}

// TestAnnotateWithoutWriterID pins the configuration rule: the hub assigns
// the writer from the device identity, and an engine without one refuses
// annotation batches instead of writing under an empty identity.
func TestAnnotateWithoutWriterID(t *testing.T) {
	hub, readers := openSeededHub(t)
	engine := NewEngine(readers, hub.Writer(), EngineOptions{})
	if _, err := engine.Annotate(context.Background(), json.RawMessage(
		`{"operations":[{"op":"delete","event_id":2,"namespace":"tags","key":"a"}]}`)); err == nil ||
		!strings.Contains(err.Error(), "writer ID") {
		t.Fatalf("missing writer error = %v", err)
	}
	var count int
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM event_annotations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("annotations = %d, want only the seeded row", count)
	}
}
