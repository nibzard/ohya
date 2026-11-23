package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	GuideGarbageAuditURI    = "ohya://guides/garbage-audit"
	GuideAnalyticsReviewURI = "ohya://guides/analytics-and-review"
)

func addGuides(server *mcp.Server, o EngineOptions) {
	guides := []struct {
		uri         string
		name        string
		title       string
		description string
		text        string
	}{
		{
			uri:         GuideGarbageAuditURI,
			name:        "garbage-audit",
			title:       "Garbage audit playbook",
			description: "Three-step FIND / MARK / EXECUTE workflow and SQL queries for auditing stale, empty, oversized, or orphaned sessions and handing ohya purge commands to the owner.",
			text:        GarbageAuditGuide(),
		},
		{
			uri:         GuideAnalyticsReviewURI,
			name:        "analytics-and-review",
			title:       "Analytics, filtering, and review pagination recipes",
			description: "SQL recipes for model and device scoping, session token totals, and paginated security-review annotation loops.",
			text:        AnalyticsReviewGuide(o),
		},
	}
	for _, g := range guides {
		uri, text := g.uri, g.text
		server.AddResource(&mcp.Resource{
			URI:         uri,
			Name:        g.name,
			Title:       g.title,
			Description: g.description,
			MIMEType:    "text/markdown",
			Size:        int64(len(text)),
		}, func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{
					{
						URI:      uri,
						MIMEType: "text/markdown",
						Text:     text,
					},
				},
			}, nil
		})
	}
}

func GarbageAuditGuide() string {
	return `Garbage audit — answer "clean up the data" in three steps: audit, mark, hand over. You cannot delete anything: the sql tool is read-only by design, so a prompt-injected transcript can never erase stored history, including evidence of itself. Find the garbage with the queries below, mark the deciding event of each garbage session with the annotate tool, then present the exact ohya purge commands for the operator to run.

Sources that stopped syncing (never committed a batch, or idle for 30 or more days):
SELECT src.id, src.agent, src.device_id, src.path, datetime(src.updated_at_ms/1000, 'unixepoch') AS updated_utc
FROM sources src
WHERE src.updated_at_ms IS NULL
   OR src.updated_at_ms < (CAST(strftime('%s','now') AS INTEGER) - 30*86400) * 1000
ORDER BY src.updated_at_ms, src.id;

Sessions whose working directory resolved to no repository — the device observed that cwd and found none; check the path before calling it garbage:
SELECT s.id, s.agent, s.device_id, s.cwd, datetime(s.last_event_at_ms/1000, 'unixepoch') AS last_utc
FROM sessions s JOIN workspaces w ON w.device_id = s.device_id AND w.path = s.cwd
WHERE w.repo IS NULL
ORDER BY s.last_event_at_ms DESC LIMIT 50;

Largest sources by mirrored bytes:
SELECT src.id, src.agent, src.device_id, src.path, src.cursor_bytes
FROM sources src ORDER BY src.cursor_bytes DESC LIMIT 20;

Oversized records inside one of those sources (over 64 KiB; substitute the source id from the query above):
SELECT e.id, e.session_id, e.kind, e.role, e.tool_name, length(e.raw_json) AS bytes
FROM events e WHERE e.source_id = 'src-3' AND length(e.raw_json) > 65536
ORDER BY bytes DESC;

Empty or single-record sessions — aborted starts and failed first calls. sessions.event_count is the maintained total, so this reads only the sessions table; synthetic ohya-synthetic companions follow their source and are never selectable:
SELECT s.id, s.agent, s.device_id, datetime(s.last_event_at_ms/1000, 'unixepoch') AS last_utc, s.event_count
FROM sessions s
WHERE s.native_id NOT LIKE 'ohya-synthetic:%' AND s.event_count <= 1
ORDER BY s.event_count, s.last_event_at_ms DESC, s.id LIMIT 50;

Replay copies inside one session you now suspect (byte-identical re-pages; 0 means none):
SELECT count(*) AS replay_copies FROM events e
WHERE e.session_id = 'claude-code:dev1:s2' AND e.duplicate_of IS NOT NULL;

Model distribution — a stale model shows as its own row with old activity; confirm before marking its sessions:
SELECT s.model, count(*) AS sessions, datetime(min(s.last_event_at_ms)/1000, 'unixepoch') AS oldest_utc, datetime(max(s.last_event_at_ms)/1000, 'unixepoch') AS newest_utc
FROM sessions s WHERE s.model IS NOT NULL GROUP BY s.model
ORDER BY min(s.last_event_at_ms), s.model;

Marking: annotate the deciding event of each session you call garbage — namespace tags, key garbage, and a value stating the reason and the evidence event id. Marks are persistent and writer-attributed, which is the owner's audit trail:
{"operations":[{"op":"upsert","event_id":42,"namespace":"tags","key":"garbage","value":{"reason":"aborted start, single record","evidence":42}}]}

Review what is marked before the owner runs anything:
SELECT DISTINCT s.id, s.agent, s.device_id, a.writer_id, datetime(s.last_event_at_ms/1000, 'unixepoch') AS last_utc
FROM event_annotations a
JOIN events e ON e.id = a.event_id
JOIN sessions s ON s.id = e.session_id
WHERE a.namespace = 'tags' AND a.key = 'garbage'
ORDER BY s.last_event_at_ms DESC;

Then present the exact commands for the operator, never run them yourself (unmark with an annotate delete first if the operator should re-check):
ohya purge --marked tags.garbage
ohya purge --marked tags.garbage --apply
The dry run prints the affected sessions, sources, event counts, and bytes; --apply deletes and tombstones each evicted source. The marked selector takes every writer's marks and combines with the other purge selectors.
`
}

func AnalyticsReviewGuide(o EngineOptions) string {
	return fmt.Sprintf(`Analytics, filtering, and review pagination recipes for the ohya sql tool.

One hit per session (dedupes noisy multi-mention sessions; keep ORDER BY + LIMIT inside the CTE so snippet() stays valid):
WITH hits AS (
  SELECT e.id, e.session_id, s.agent, s.cwd, datetime(e.time_ms/1000, 'unixepoch') AS utc_time, events_fts.rank AS rk, snippet(events_fts, 0, '[', ']', '...', 24) AS excerpt
  FROM events_fts JOIN events_settled e ON e.id = events_fts.rowid JOIN sessions s ON s.id = e.session_id
  WHERE events_fts MATCH 'authentication AND refresh' ORDER BY events_fts.rank LIMIT 100)
SELECT id, session_id, agent, cwd, utc_time, excerpt, min(rk) AS best_rank FROM hits GROUP BY session_id ORDER BY best_rank LIMIT 10;

One model's sessions (discover values first with SELECT DISTINCT model FROM sessions; NULL means the records carried no model):
SELECT s.id, s.agent, s.device_id, substr(s.first_prompt, 1, 160) AS first_prompt, datetime(s.last_event_at_ms/1000, 'unixepoch') AS last_utc
FROM sessions s WHERE s.model = 'claude-sonnet-4-5'
ORDER BY s.last_event_at_ms DESC LIMIT 10;

One device's sessions (discover values first with SELECT DISTINCT device_id FROM sessions; name the device of each session you cite):
SELECT s.id, s.agent, s.repo, substr(s.first_prompt, 1, 160) AS first_prompt, datetime(s.last_event_at_ms/1000, 'unixepoch') AS last_utc
FROM sessions s WHERE s.device_id = 'laptop'
ORDER BY s.last_event_at_ms DESC LIMIT 10;

Session token totals:
SELECT s.id, sum(u.input_tokens) AS input_tokens, sum(u.output_tokens) AS output_tokens
FROM sessions s LEFT JOIN session_token_usage u ON u.session_id = s.id
GROUP BY s.id ORDER BY input_tokens DESC LIMIT 10;

Events still missing your security review (page by id; bind your writer ID as a literal):
SELECT e.id FROM events e
WHERE e.id > 100 AND NOT EXISTS (
  SELECT 1 FROM event_annotations a WHERE a.event_id = e.id
  AND a.writer_id = '%s' AND a.namespace = 'security' AND a.key = 'review'
  AND json_extract(a.value, '$.status') = 'reviewed')
ORDER BY e.id LIMIT 100;
`, o.WriterID)
}
