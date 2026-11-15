package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type runResult struct {
	ok        bool
	answer    string
	toolCalls []string
	problems  []string
	notes     []string
	traceDir  string
}

func (g *gate) runClaudeCase(ctx context.Context, c caseDef) *runResult {
	dir := filepath.Join(g.tracesDir, "claude-code-"+c.key)
	res := &runResult{traceDir: dir}
	if err := os.MkdirAll(filepath.Join(dir, "cwd"), 0o700); err != nil {
		res.problems = append(res.problems, "prepare case dir: "+err.Error())
		return res
	}
	streamPath := filepath.Join(dir, "stream.jsonl")
	stderrPath := filepath.Join(dir, "stderr.txt")
	stream, err := os.Create(streamPath)
	if err != nil {
		res.problems = append(res.problems, err.Error())
		return res
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		stream.Close()
		res.problems = append(res.problems, err.Error())
		return res
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()

	cmd := exec.CommandContext(ctx, g.claude,
		"-p", c.prompt,
		"--mcp-config", filepath.Join(g.out, "claude", "mcp.json"),
		"--strict-mcp-config",
		"--allowed-tools", "mcp__ohya__sql,mcp__ohya__annotate",
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Dir = filepath.Join(dir, "cwd")
	cmd.Env = g.claudeEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, stream, stderr
	cmd.WaitDelay = 15 * time.Second
	runErr := cmd.Run()
	stream.Close()
	stderr.Close()

	res.answer, res.ok, res.toolCalls, res.problems = parseClaudeTrace(streamPath)
	if ctx.Err() != nil {
		res.problems = append(res.problems, "claude run timed out or was interrupted: "+ctx.Err().Error())
	} else if runErr != nil && !res.ok {
		res.problems = append(res.problems, "claude exited with error: "+runErr.Error())
	}
	return res
}

func (g *gate) runCodexCase(ctx context.Context, c caseDef) *runResult {
	dir := filepath.Join(g.tracesDir, "codex-"+c.key)
	res := &runResult{traceDir: dir}
	if err := os.MkdirAll(filepath.Join(dir, "cwd"), 0o700); err != nil {
		res.problems = append(res.problems, "prepare case dir: "+err.Error())
		return res
	}
	eventsPath := filepath.Join(dir, "events.jsonl")
	stderrPath := filepath.Join(dir, "stderr.txt")
	lastPath := filepath.Join(dir, "last-message.txt")
	events, err := os.Create(eventsPath)
	if err != nil {
		res.problems = append(res.problems, err.Error())
		return res
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		events.Close()
		res.problems = append(res.problems, err.Error())
		return res
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()

	cmd := exec.CommandContext(ctx, g.codex, "exec",
		"--skip-git-repo-check",
		"--ephemeral",
		"--trust",
		"--json",
		"-o", lastPath,
		c.prompt,
	)
	cmd.Dir = filepath.Join(dir, "cwd")
	cmd.Env = g.codexEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, events, stderr
	cmd.WaitDelay = 15 * time.Second
	runErr := cmd.Run()
	events.Close()
	stderr.Close()

	lastMsg, turnDone, calls, problems, notes := parseCodexTrace(eventsPath)
	res.toolCalls, res.problems, res.notes = calls, problems, notes
	res.answer = lastMsg
	if data, err := os.ReadFile(lastPath); err == nil && strings.TrimSpace(string(data)) != "" {
		res.answer = strings.TrimSpace(string(data))
	}
	res.ok = turnDone && res.answer != ""
	if ctx.Err() != nil {
		res.problems = append(res.problems, "codex run timed out or was interrupted: "+ctx.Err().Error())
	} else if runErr != nil && !res.ok {
		res.problems = append(res.problems, "codex exited with error: "+runErr.Error())
	}
	if !turnDone {
		res.problems = append(res.problems, "codex trace has no turn.completed event")
	}
	return res
}

func (g *gate) claudeEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.claudeHome,
		"TERM=dumb",
		"LANG=C.UTF-8",
		"CLAUDE_CONFIG_DIR=" + g.claudeConfig,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANTHROPIC_") {
			env = append(env, kv)
		}
	}
	return env
}

func (g *gate) codexEnv() []string {
	home := os.Getenv("HOME")
	if home == "" {
		home = g.out
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TERM=dumb",
		"LANG=C.UTF-8",
		"CODEX_HOME=" + g.codexHome,
	}
}

func parseClaudeTrace(path string) (answer string, ok bool, calls []string, problems []string) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, nil, []string{"open trace: " + err.Error()}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	resultSeen := false
	for sc.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Result  string `json:"result"`
			Message *struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			if ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "tool_use" {
						calls = append(calls, block.Name)
					}
				}
			}
		case "result":
			resultSeen = true
			answer, ok = ev.Result, ev.Subtype == "success"
			if !ok {
				problems = append(problems, "claude result subtype "+ev.Subtype+": "+truncate(ev.Result, 200))
			}
		}
	}
	if err := sc.Err(); err != nil {
		problems = append(problems, "read trace: "+err.Error())
	}
	if !resultSeen {
		problems = append(problems, "trace has no result event")
	}
	return answer, ok, calls, problems
}

func parseCodexTrace(path string) (lastMessage string, turnDone bool, calls []string, problems []string, notes []string) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, nil, []string{"open trace: " + err.Error()}, nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Item *struct {
				Type    string `json:"type"`
				Server  string `json:"server"`
				Tool    string `json:"tool"`
				Text    string `json:"text"`
				Message string `json:"message"`
			} `json:"item"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item == nil {
				continue
			}
			switch ev.Item.Type {
			case "mcp_tool_call":
				name := ev.Item.Server + "." + ev.Item.Tool
				calls = append(calls, name)
			case "agent_message":
				lastMessage = ev.Item.Text
			case "error":
				item := "codex error item: " + truncate(ev.Item.Message, 160)
				if benignCodexErrorItem(ev.Item.Message) {
					notes = append(notes, item)
				} else {
					problems = append(problems, item)
				}
			}
		case "turn.completed":
			turnDone = true
		}
	}
	if err := sc.Err(); err != nil {
		problems = append(problems, "read trace: "+err.Error())
	}
	return lastMessage, turnDone, calls, problems, notes
}

const webSocketFallbackItem = "Falling back from WebSockets to HTTPS transport."

func benignCodexErrorItem(message string) bool {
	return strings.HasPrefix(message, webSocketFallbackItem)
}

var evidenceRe = regexp.MustCompile(`(?i)evidence"?:?\s*#?\s*(\d+)`)

func citedEventID(answer string) (int64, bool) {
	matches := evidenceRe.FindAllStringSubmatch(answer, -1)
	if len(matches) == 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(matches[len(matches)-1][1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func checkCase(c caseDef, r *runResult, resolve resolveFunc) (problems []string) {
	problems = append(problems, r.problems...)
	for _, want := range c.answerContains {
		if !strings.Contains(r.answer, want) {
			problems = append(problems, fmt.Sprintf("answer does not contain %q", want))
		}
	}
	var ohya []string
	for _, call := range r.toolCalls {
		if strings.Contains(call, "ohya") {
			ohya = append(ohya, call)
		}
	}
	if c.requireSQL {
		hasSQL := false
		for _, call := range ohya {
			if strings.Contains(call, "sql") {
				hasSQL = true
			}
		}
		if !hasSQL {
			problems = append(problems, fmt.Sprintf("the client never queried memory through the ohya sql tool (tool calls: %s)", callsOrNone(r.toolCalls)))
		}
	}
	if c.forbidSQL && len(ohya) > 0 {
		problems = append(problems, fmt.Sprintf("the client queried memory despite the no-query instruction (tool calls: %s)", strings.Join(ohya, ", ")))
	}
	if c.evidenceMarker != "" {
		id, ok := citedEventID(r.answer)
		if !ok {
			problems = append(problems, "answer cites no event id (expected 'evidence: <event-id>')")
		} else {
			text, found, err := resolve(context.Background(), id)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("resolve cited event %d: %v", id, err))
			case !found:
				problems = append(problems, fmt.Sprintf("cited event id %d does not resolve in the hub", id))
			case !strings.Contains(text, c.evidenceMarker):
				problems = append(problems, fmt.Sprintf("cited event %d does not carry the expected evidence %q (text: %s)", id, c.evidenceMarker, truncate(text, 120)))
			}
		}
	}
	return problems
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + " …"
	}
	if s == "" {
		return "(empty)"
	}
	return s
}
