package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nibzard/ohya/internal/mcpserver"
)

func SkillPath(o ClientOptions, c Client) (string, error) {
	var dir string
	switch c.Name {
	case "claude":
		getenv := o.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		dir = getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			dir = filepath.Join(o.Home, ".claude")
		}
	case "codex", "pi", "agy":
		if c.Config == "" {
			return "", fmt.Errorf("%s configuration path is empty", c.Name)
		}
		dir = filepath.Dir(c.Config)
	default:
		return "", fmt.Errorf("unsupported client %s", c.Name)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s skill root %q must be absolute", c.Name, dir)
	}
	return filepath.Join(dir, "skills", "ohya", "SKILL.md"), nil
}

func SkillContent() []byte {
	var b strings.Builder
	b.WriteString(`---
name: ohya
description: >-
  Shared local-first memory across Claude Code, Codex, Pi, and Antigravity sessions
  on all paired devices. Use when recalling prior decisions, handoffs, debugging
  history, unfinished work, cross-device context, or auditing and marking stale
  ohya history for owner purge.
---

# ohya — Shared Agent Memory

`)
	b.WriteString(mcpserver.MemoryInstructions)
	b.WriteString(`

## Querying ohya

- **Interactive recall (default):** call the MCP ` + "`sql`" + ` and ` + "`annotate`" + ` tools.
- **Scripted / multi-step pipelines:** run ` + "`ohya sql --json '<query>'`" + ` or ` + "`ohya sql --json - < query.sql`" + ` (add ` + "`--remote`" + ` on a joined device). It runs through the same read-only SQLite authorizer, 500-row / 256 KiB / 5 s bounds, and egress secret redaction as the MCP tool. Never open ` + "`hub.db`" + ` directly with ` + "`sqlite3`" + `.
- **Keep results small & fast:** use ` + "`snippet(events_fts, 0, '[', ']', '...', 24)`" + ` for FTS matches, ` + "`substr(first_prompt, 1, 160)`" + ` for session lists, ` + "`datetime(col/1000, 'unixepoch')`" + ` for readable UTC times, ` + "`session_id = '...'`" + ` (indexed; never ` + "`LIKE '%...'`" + `), and covering subqueries over ` + "`events_settled`" + ` before sorting by ` + "`time_ms`" + `.
- **FTS5 quoting:** wrap punctuation-bearing terms (hyphens, slashes, apostrophes) in double quotes such as ` + "`'\"auth-refresh\"'`" + `.

## Core Recall Recipes

Find this project's sessions across all devices:

` + "```sql" + `
SELECT device_id, agent, cwd, substr(first_prompt, 1, 160) AS first_prompt, datetime(last_event_at_ms/1000, 'unixepoch') AS last_utc
FROM sessions WHERE repo = 'github.com/owner/name'
ORDER BY last_event_at_ms DESC LIMIT 10;
` + "```" + `

Find sessions by agent and time window (` + "`claude-code`" + `, ` + "`codex`" + `, ` + "`pi`" + `, ` + "`antigravity`" + `):

` + "```sql" + `
SELECT s.id, s.device_id, s.repo, substr(s.first_prompt, 1, 160) AS first_prompt, datetime(s.last_event_at_ms/1000, 'unixepoch') AS last_utc
FROM sessions s
WHERE s.agent = 'claude-code'
  AND s.last_event_at_ms >= (CAST(strftime('%s','now') AS INTEGER) - 7*86400) * 1000
ORDER BY s.last_event_at_ms DESC LIMIT 20;
` + "```" + `

Full-text search with centered match snippet:

` + "```sql" + `
SELECT e.id, e.session_id, s.agent, s.cwd, datetime(e.time_ms/1000, 'unixepoch') AS utc_time, snippet(events_fts, 0, '[', ']', '...', 24) AS excerpt
FROM events_fts JOIN events_settled e ON e.id = events_fts.rowid
JOIN sessions s ON s.id = e.session_id
WHERE events_fts MATCH 'authentication AND refresh'
ORDER BY events_fts.rank LIMIT 10;
` + "```" + `

Read surrounding records around a hit in the same session:

` + "```sql" + `
SELECT e.id, datetime(e.time_ms/1000, 'unixepoch') AS utc_time, e.kind, e.role, e.tool_name, substr(e.text, 1, 400) AS excerpt
FROM events e
WHERE e.session_id = 'claude-code:dev1:s1'
  AND e.duplicate_of IS NULL
  AND e.id BETWEEN 2-5 AND 2+5
ORDER BY e.id;
` + "```" + `

Count matching sessions and events in one pass:

` + "```sql" + `
SELECT count(DISTINCT e.session_id) AS sessions, count(*) AS events
FROM events_fts JOIN events_settled e ON e.id = events_fts.rowid
WHERE events_fts MATCH 'authentication AND refresh';
` + "```" + `

## Garbage Audit Playbook

`)
	b.WriteString(mcpserver.GarbageAuditGuide())
	b.WriteString("\n## Analytics and Security Review Playbook\n\n")
	b.WriteString(mcpserver.AnalyticsReviewGuide(mcpserver.EngineOptions{WriterID: "<your-writer-id>"}))
	return []byte(b.String())
}

func ApplySkill(o ClientOptions, c Client) (bool, error) {
	path, err := SkillPath(o, c)
	if err != nil {
		return false, err
	}
	receipt := filepath.Join(o.State, "setup", "skills", c.Name+".json")
	return ApplyFile(path, receipt, SkillContent())
}

func RemoveSkill(state, name string) (bool, error) {
	if name != "claude" && name != "codex" && name != "pi" && name != "agy" {
		return false, fmt.Errorf("unsupported client %s", name)
	}
	receipt := filepath.Join(state, "setup", "skills", name+".json")
	r, err := loadFileReceipt(receipt)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	changed, err := RemoveFile(receipt)
	if err != nil {
		return changed, err
	}
	_ = os.Remove(filepath.Dir(r.Path))
	return changed, nil
}
