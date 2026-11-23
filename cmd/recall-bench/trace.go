package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
)

type traceMetrics struct {
	Answer          string
	OK              bool
	Model           string
	ToolCalls       []string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	CacheNewTokens  int64
	ToolResultBytes int64
	EmptyResults    int
	ErrorResults    int
	DeniedCalls     int
	DurationMS      int64
	NumTurns        int
	CostUSD         float64
	Problems        []string
}

type streamEvent struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	IsError bool    `json:"is_error"`
	Result  string  `json:"result"`
	Model   string  `json:"model"`
	DurMS   int64   `json:"duration_ms"`
	Turns   int     `json:"num_turns"`
	CostUSD float64 `json:"total_cost_usd"`
	Usage   *struct {
		Input     int64 `json:"input_tokens"`
		CacheNew  int64 `json:"cache_creation_input_tokens"`
		CacheRead int64 `json:"cache_read_input_tokens"`
		Output    int64 `json:"output_tokens"`
	} `json:"usage"`
	ModelUsage        map[string]json.RawMessage `json:"modelUsage"`
	PermissionDenials []struct {
		ToolUseID string `json:"tool_use_id"`
	} `json:"permission_denials"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input     int64 `json:"input_tokens"`
			CacheNew  int64 `json:"cache_creation_input_tokens"`
			CacheRead int64 `json:"cache_read_input_tokens"`
			Output    int64 `json:"output_tokens"`
		} `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type assistantBlock struct {
	Type string `json:"type"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

type contentBlock struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type usageTotals struct {
	Input     int64
	Output    int64
	CacheRead int64
	CacheNew  int64
}

func parseClaudeStream(path string) traceMetrics {
	var m traceMetrics
	f, err := os.Open(path)
	if err != nil {
		m.Problems = append(m.Problems, "open trace: "+err.Error())
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	msgUsage := map[string]usageTotals{}
	var callIDs []string
	resultSeen := false
	resultHasUsage := false
	for sc.Scan() {
		var ev streamEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			if ev.Message == nil {
				continue
			}
			if ev.Message.Model != "" {
				m.Model = ev.Message.Model
			}
			if ev.Message.Usage != nil && ev.Message.ID != "" {
				u := msgUsage[ev.Message.ID]
				if ev.Message.Usage.Input > u.Input {
					u.Input = ev.Message.Usage.Input
				}
				if ev.Message.Usage.Output > u.Output {
					u.Output = ev.Message.Usage.Output
				}
				if ev.Message.Usage.CacheRead > u.CacheRead {
					u.CacheRead = ev.Message.Usage.CacheRead
				}
				if ev.Message.Usage.CacheNew > u.CacheNew {
					u.CacheNew = ev.Message.Usage.CacheNew
				}
				msgUsage[ev.Message.ID] = u
			}
			var blocks []assistantBlock
			if err := json.Unmarshal(ev.Message.Content, &blocks); err == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" {
						m.ToolCalls = append(m.ToolCalls, b.Name)
						callIDs = append(callIDs, b.ID)
					}
				}
			}
		case "user":
			if ev.Message != nil {
				accumulateToolResult(&m, ev.Message.Content)
			}
		case "result":
			resultSeen = true
			m.Answer, m.OK = ev.Result, ev.Subtype == "success" && !ev.IsError
			m.DurationMS, m.NumTurns = ev.DurMS, ev.Turns
			m.CostUSD = ev.CostUSD
			if ev.Model != "" {
				m.Model = ev.Model
			}
			if ev.Usage != nil && (ev.Usage.Input > 0 || ev.Usage.Output > 0 ||
				ev.Usage.CacheRead > 0 || ev.Usage.CacheNew > 0) {
				resultHasUsage = true
				m.InputTokens = ev.Usage.Input
				m.OutputTokens = ev.Usage.Output
				m.CacheReadTokens = ev.Usage.CacheRead
				m.CacheNewTokens = ev.Usage.CacheNew
			}
			if m.Model == "" && len(ev.ModelUsage) > 0 {
				models := make([]string, 0, len(ev.ModelUsage))
				for name := range ev.ModelUsage {
					models = append(models, name)
				}
				sort.Strings(models)
				m.Model = strings.Join(models, ",")
			}
			if len(ev.PermissionDenials) > 0 {
				denied := map[string]bool{}
				for _, d := range ev.PermissionDenials {
					denied[d.ToolUseID] = true
				}
				var kept []string
				for i, id := range callIDs {
					if !denied[id] {
						kept = append(kept, m.ToolCalls[i])
					}
				}
				m.ToolCalls = kept
				m.DeniedCalls = len(callIDs) - len(kept)
			}
			if !m.OK {
				m.Problems = append(m.Problems, "claude result subtype "+ev.Subtype+": "+truncate(ev.Result, 200))
			}
		}
	}
	if !resultHasUsage {
		for _, u := range msgUsage {
			m.InputTokens += u.Input
			m.OutputTokens += u.Output
			m.CacheReadTokens += u.CacheRead
			m.CacheNewTokens += u.CacheNew
		}
	}
	if err := sc.Err(); err != nil {
		m.Problems = append(m.Problems, "read trace: "+err.Error())
	}
	if !resultSeen {
		m.Problems = append(m.Problems, "trace has no result event")
	}
	return m
}

func accumulateToolResult(m *traceMetrics, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var blocks []contentBlock
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return
		}
	} else {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		text := toolResultText(b.Content)
		m.ToolResultBytes += int64(len(text))
		if b.IsError {
			m.ErrorResults++
		} else if strings.TrimSpace(text) == "" {
			m.EmptyResults++
		}
	}
}

func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	case '[':
		var blocks []textBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return ""
		}
		var parts []string
		for _, b := range blocks {
			parts = append(parts, b.Text)
		}
		return strings.Join(parts, "\n")
	}
	return ""
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
