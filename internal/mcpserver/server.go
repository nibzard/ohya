package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nibzard/ohya/internal/store"
)

// ServerVersion is the implementation version advertised in the MCP
// initialize handshake. It tracks the repository state, not a release.
const ServerVersion = "0.1.0-dev"

// ToolName is the read-only query tool's name.
const ToolName = "sql"

// Backend serves the two tool operations behind the MCP surface. The local
// engine over the hub database implements it, and the device-side bridge of
// the sync protocol implements it over one authenticated hub connection
// (internal/hubsync.RemoteSession), so local and remote agents see the same
// tools under the same bounds. The annotate writer always comes from the
// caller's identity, never from request content.
type Backend interface {
	// Query runs exactly one bounded read-only statement and returns its
	// truncated-to-limits result.
	Query(ctx context.Context, query string) (Result, error)
	// AnnotateAs applies one annotate request's JSON arguments, attributing
	// every operation of the batch to writerID.
	AnnotateAs(ctx context.Context, writerID string, raw json.RawMessage) (store.AnnotateResult, error)
}

// The engine serves the tools over the hub database.
var _ Backend = (*Engine)(nil)

// New returns an MCP server serving the read-only sql(query) tool and the
// annotate(operations) tool over one backend: the local engine, or a remote
// session on a joined device. Callers run it with Run, typically over stdio.
func New(backend Backend, o EngineOptions) *mcp.Server {
	o = o.withDefaults()
	server := mcp.NewServer(&mcp.Implementation{Name: "ohya", Version: ServerVersion}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolName,
		Title:       "Query the ohya transcript database",
		Description: ToolDescription(o),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args sqlArgs) (*mcp.CallToolResult, Result, error) {
		result, err := backend.Query(ctx, args.Query)
		if err != nil {
			return nil, Result{}, fmt.Errorf("sql tool: %w", err)
		}
		return nil, result, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        AnnotateToolName,
		Title:       "Annotate events",
		Description: AnnotateDescription(o),
		Annotations: &mcp.ToolAnnotations{
			// The tool writes, and a delete discards a value, so it may be
			// destructive; only event_annotations is ever touched. A retried
			// batch converges: identical operations are no-ops.
			ReadOnlyHint:   false,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ annotateArgs) (*mcp.CallToolResult, store.AnnotateResult, error) {
		result, err := backend.AnnotateAs(ctx, o.WriterID, req.Params.Arguments)
		if err != nil {
			return nil, store.AnnotateResult{}, fmt.Errorf("annotate tool: %w", err)
		}
		return nil, result, nil
	})
	return server
}

// Run serves the tool over stdio until ctx is done or stdin closes.
func Run(ctx context.Context, server *mcp.Server) error {
	return server.Run(ctx, &mcp.StdioTransport{})
}

// sqlArgs is the tool's input: one query per call.
type sqlArgs struct {
	Query string `json:"query" jsonschema:"one SQL SELECT statement (or read-only PRAGMA); exactly one statement, no semicolon-separated batches"`
}

// ToolDescription publishes the schema, SQL examples, the limits, and the
// current writer ID, so an agent can query without any out-of-band context.
func ToolDescription(o EngineOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, `Run one read-only SQL statement against the ohya agent-transcript database and return its rows as JSON.

Policy: exactly one statement per call (no semicolon-separated batches). SELECT and the listed read-only PRAGMAs only; writes, schema changes, ATTACH, extension loading and state-changing pragmas are denied by a SQLite authorizer on a read-only connection. Every query is bounded: %d rows, %d bytes of row JSON, a %s execution deadline, %d concurrent queries. Rows cut by a limit are reported with "truncated": true and a truncation_reason, never silently.

Current writer ID (use as event_annotations.writer_id and for your own annotations): %s

Tables:
- sessions(id, agent, device_id, native_id, repo, cwd, branch, model, first_prompt, started_at_ms, last_event_at_ms, event_count, metadata): one agent session; id = agent + ":" + device_id + ":" + native_id
- sources(id, device_id, agent, path, file_identity, cursor_bytes, updated_at_ms): one watched file incarnation and its committed byte cursor
- events(id, session_id, source_id, source_offset, time_ms, kind, role, tool_name, text, raw_json, metadata, ordinal, duplicate_of): one immutable original JSONL record; text is the normalized searchable text; ordinal is the agent's per-session record number (Codex), and duplicate_of names the first stored copy of a record that another file replayed byte-identically (its text is NULL; filter with duplicate_of IS NULL). A file that re-enumerates an ordinal after a rollback holds a different record there; it is stored as another original, so one ordinal can name several distinct records
- session_token_usage(session_id, message_id, input_tokens, cache_creation_input_tokens, cache_read_input_tokens, output_tokens): token counts of one API response
- event_annotations(event_id, namespace, key, writer_id, value, created_at_ms, updated_at_ms): agent-written JSON values, identity (event_id, namespace, key, writer_id)
- events_fts: FTS5 full-text index over events.text; rowid equals events.id
- schema_migrations(version, name, checksum, applied_at_ms): applied schema history

Schema inspection: SELECT sql FROM sqlite_master WHERE type = 'table'; and the read-only PRAGMAs table_info, table_xinfo, table_list, index_list, index_info, index_xinfo, foreign_key_list, database_list, function_list, module_list, pragma_list, collation_list. EXPLAIN QUERY PLAN works for checking index use.

Example — recent sessions:
SELECT id, agent, first_prompt, last_event_at_ms, event_count FROM sessions ORDER BY last_event_at_ms DESC LIMIT 5;

Example — full-text search (BM25 rank):
SELECT e.id, s.cwd, e.time_ms, substr(e.text, 1, 300) AS excerpt
FROM events_fts JOIN events e ON e.id = events_fts.rowid
JOIN sessions s ON s.id = e.session_id
WHERE events_fts MATCH 'authentication AND refresh'
ORDER BY events_fts.rank LIMIT 10;

Example — inspect one record by id:
SELECT id, kind, role, tool_name, time_ms, raw_json FROM events WHERE id = 42;

Example — session token totals:
SELECT s.id, sum(u.input_tokens) AS input_tokens, sum(u.output_tokens) AS output_tokens
FROM sessions s LEFT JOIN session_token_usage u ON u.session_id = s.id
GROUP BY s.id ORDER BY input_tokens DESC LIMIT 10;

Example — events still missing your security review (page by id, bind your writer ID as a literal):
SELECT e.id FROM events e
WHERE e.id > 100 AND NOT EXISTS (
  SELECT 1 FROM event_annotations a WHERE a.event_id = e.id
  AND a.writer_id = '%s' AND a.namespace = 'security' AND a.key = 'review'
  AND json_extract(a.value, '$.status') = 'reviewed')
ORDER BY e.id LIMIT 100;
`, o.MaxRows, o.MaxOutputBytes, o.Deadline, o.MaxConcurrency, o.WriterID, o.WriterID)
	return b.String()
}
