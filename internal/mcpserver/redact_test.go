package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nibzard/ohya/internal/secrets"
)

func seedSecretEvent(t *testing.T) *Engine {
	t.Helper()
	hub, readers := openSeededHub(t)
	secretText := "export GITHUB_TOKEN=ghp_" + strings.Repeat("f", 36) +
		" and the aws key AKIAIOSFODNN7EXAMPLE"
	raw := `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":"` +
		secretText + `"}}`
	for _, q := range []string{
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json)
		 VALUES(3,'claude:dev1:sess-1','src-1',100,3000,'message','user',` + quoteSQL(secretText) + `,` + quoteSQL(raw) + `)`,
	} {
		if _, err := hub.Writer().Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	return NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1"})
}

func quoteSQL(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func TestQueryRedactsSecretShapedValues(t *testing.T) {
	e := seedSecretEvent(t)

	r := mustQuery(t, e, `SELECT id, text, raw_json FROM events WHERE id = 3`)
	if r.RowCount != 1 || len(r.Rows) != 1 {
		t.Fatalf("result = %+v", r)
	}
	row := r.Rows[0]
	if row[0] != int64(3) {
		t.Fatalf("numeric cell = %v; want the untouched id", row[0])
	}
	text, _ := row[1].(string)
	raw, _ := row[2].(string)
	for _, cell := range []string{text, raw} {
		if strings.Contains(cell, "ghp_") || strings.Contains(cell, "AKIAIOSFODNN7EXAMPLE") {
			t.Fatalf("cell still holds secret text: %q", cell)
		}
		if !strings.Contains(cell, secrets.Marker("github-token")) ||
			!strings.Contains(cell, secrets.Marker("aws-access-key-id")) {
			t.Fatalf("cell lacks kind-labeled markers: %q", cell)
		}
	}
	if want := "export GITHUB_TOKEN=" + secrets.Marker("github-token"); !strings.Contains(text, want) {
		t.Fatalf("text lost the variable name: %q", text)
	}
	if r.Redactions != 4 {
		t.Fatalf("redactions = %d; want 4 (two cells, two matches each)", r.Redactions)
	}
}

func TestQueryLeavesOrdinaryIdentifiersAlone(t *testing.T) {
	hub, readers := openSeededHub(t)
	const identifier = "task-add-device-revocation-regression-tests"
	if _, err := hub.Writer().Exec(
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json)
		 VALUES(3,'claude:dev1:sess-1','src-1',100,3000,'message','user',` + quoteSQL(identifier) + `,'{}')`); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1"})
	r := mustQuery(t, e, `SELECT text FROM events WHERE id = 3`)
	if got, _ := r.Rows[0][0].(string); got != identifier || r.Redactions != 0 {
		t.Fatalf("identifier came back as %q with %d redactions; want the identifier and 0", got, r.Redactions)
	}
}

func TestQueryRedactionServesThroughTheMcpTool(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	hub, readers := openSeededHub(t)
	secretText := "ANTHROPIC_API_KEY=sk-ant-api03-" + strings.Repeat("C", 40)
	if _, err := hub.Writer().Exec(
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json)
		 VALUES(3,'claude:dev1:sess-1','src-1',100,3000,'message','user',` + quoteSQL(secretText) + `, '{}')`); err != nil {
		t.Fatal(err)
	}
	server := New(NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1"}), EngineOptions{WriterID: "dev1"})
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "redact-test", Version: "v0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: ToolName, Arguments: map[string]any{"query": "SELECT text FROM events WHERE id = 3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload Result
	if err := redecode(result, &payload); err != nil {
		t.Fatal(err)
	}
	got, _ := payload.Rows[0][0].(string)
	if strings.Contains(got, "sk-ant-") {
		t.Fatalf("tool result still holds the key: %q", got)
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY="+secrets.Marker("anthropic-api-key")) {
		t.Fatalf("tool result = %q; want the kept name and the marker", got)
	}
	if payload.Redactions != 1 {
		t.Fatalf("redactions = %d; want 1", payload.Redactions)
	}
}

func TestQueryRedactionCountsSurviveTheWireShape(t *testing.T) {
	r := Result{Columns: []string{"text"}, Rows: [][]any{{"[redacted:slack-token]"}},
		RowCount: 1, Redactions: 2}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"redactions":2`) {
		t.Fatalf("encoded result = %s; want the redactions count", encoded)
	}
	var back Result
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != string(encoded) {
		t.Fatalf("round trip:\n got %s\nwant %s", reencoded, encoded)
	}
	clean := Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}, RowCount: 1}
	encoded, err = json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "redactions") {
		t.Fatalf("zero-redaction result carries the field: %s", encoded)
	}
}
