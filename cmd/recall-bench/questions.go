package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Question struct {
	Key              string   `json:"key"`
	Tier             string   `json:"tier"`
	Scope            string   `json:"scope"`
	Prompt           string   `json:"prompt"`
	AnswerContains   []string `json:"answer_contains"`
	CountValue       int      `json:"count_value,omitempty"`
	Negative         bool     `json:"negative,omitempty"`
	EvidenceMarker   string   `json:"evidence_marker,omitempty"`
	ExpectedSessions int      `json:"expected_sessions,omitempty"`
	ExpectedSettled  int      `json:"expected_settled,omitempty"`
	ExpectedFiles    int      `json:"expected_files,omitempty"`
}

var knownTiers = map[string]bool{
	"needle":      true,
	"paraphrase":  true,
	"timeboxed":   true,
	"correction":  true,
	"aggregate":   true,
	"multihop":    true,
	"negative":    true,
	"crossdevice": true,
}

func loadQuestions(paths []string) ([]Question, error) {
	var all []Question
	seen := map[string]bool{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var qs []Question
		if err := json.Unmarshal(data, &qs); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for i, q := range qs {
			if err := validateQuestion(q); err != nil {
				return nil, fmt.Errorf("%s question %d: %w", p, i, err)
			}
			if seen[q.Key] {
				return nil, fmt.Errorf("%s: duplicate question key %q", p, q.Key)
			}
			seen[q.Key] = true
			all = append(all, qs[i])
		}
	}
	return all, nil
}

func validateQuestion(q Question) error {
	if q.Key == "" {
		return errors.New("empty key")
	}
	if !knownTiers[q.Tier] {
		return fmt.Errorf("unknown tier %q", q.Tier)
	}
	if q.Scope != "" && q.Scope != "local" && q.Scope != "remote-only" {
		return fmt.Errorf("scope must be local or remote-only, got %q", q.Scope)
	}
	if q.Prompt == "" {
		return errors.New("empty prompt")
	}
	if strings.ContainsAny(q.EvidenceMarker, "\"'`\n;") {
		return fmt.Errorf("evidence marker %q contains a character the graders or FTS quoting cannot carry", q.EvidenceMarker)
	}
	if q.Negative {
		if len(q.AnswerContains) > 0 {
			return errors.New("negative question must not define answer_contains")
		}
		if q.ExpectedSessions != 0 || q.ExpectedFiles != 0 {
			return errors.New("negative question expects zero sessions and files; preflight enforces it")
		}
		return nil
	}
	if q.Tier == "aggregate" {
		if q.CountValue < 1 {
			return errors.New("aggregate question needs count_value >= 1")
		}
		return nil
	}
	if len(q.AnswerContains) == 0 {
		return errors.New("non-negative question needs answer_contains")
	}
	if q.EvidenceMarker == "" {
		return errors.New("non-negative question needs an evidence marker")
	}
	if q.ExpectedSessions < 1 {
		return errors.New("non-negative question needs expected_sessions >= 1")
	}
	if (q.Scope == "" || q.Scope == "local") && q.ExpectedFiles < 1 {
		return errors.New("local question needs expected_files >= 1")
	}
	return nil
}

const (
	rawPreamble = "You are recalling facts recorded in past coding-agent sessions on this machine. " +
		"Your only source is the on-disk transcript archives: Claude Code transcripts under %s " +
		"(one directory per project, one JSONL file per session), Codex transcripts under %s " +
		"(JSONL rollout files under year/month/day directories), and Pi transcripts under %s " +
		"(JSONL files under an encoded-cwd directory). You have shell, grep, glob, and file-read tools."

	ohyaPreamble = "You are recalling facts recorded in past coding-agent sessions through the ohya " +
		"shared-memory server, available as the mcp__ohya__sql tool: read-only SQL over a database " +
		"of agent sessions and events. The tool description documents the schema and recall recipes. " +
		"Use only that tool."

	answerShape = " Answer concisely, in at most three sentences."
)

func armPrompt(q Question, arm string, roots []string) string {
	var body string
	if arm == armRaw {
		body = fmt.Sprintf(rawPreamble, roots[0], roots[1], roots[2])
	} else {
		body = ohyaPreamble
	}
	instructions := instructionsFor(q, arm)
	return body + "\n\nQuestion: " + q.Prompt + "\n\n" + instructions + answerShape
}

func instructionsFor(q Question, arm string) string {
	var evidence string
	if arm == armRaw {
		evidence = "End your reply with exactly one line of the form 'evidence: <path>' where <path> " +
			"is the absolute path of the transcript file that contains the deciding record."
	} else {
		evidence = "End your reply with exactly one line of the form 'evidence: <id>' where <id> " +
			"is the numeric events.id of the event that contains the deciding record."
	}
	switch {
	case q.Negative:
		return "If the fact was recorded anywhere, report it and cite the evidence as described next. " +
			"If you find no record of it, state plainly that no evidence was found and end with " +
			"exactly the line 'evidence: none' instead. " + evidence
	case q.Tier == "aggregate":
		return "End your reply with exactly one line of the form 'count: N' where N is the number " +
			"of distinct sessions that satisfy the question."
	default:
		return evidence
	}
}

func canaryQuestionsPath(outDir string) string {
	return filepath.Join(outDir, "canary-questions.json")
}
