package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
)

const (
	maxSessionStartInput = 64 * 1024
	sessionStartReminder = "When the user asks to continue prior work, recover a handoff, or revisit a past decision, use Ohya to search this project's history. Verify findings against the current checkout before acting."
)

var sessionStartSpec = commandSpec{
	name:        "ohya session-start",
	synopsis:    "provide an opt-in recall reminder to an agent hook",
	usageLine:   "ohya session-start --harness codex|claude",
	description: "Reads a SessionStart event from stdin and returns a fixed recall reminder for a fresh session. Configure it in the workspace where you want the reminder.",
	examples:    []string{"ohya session-start --harness codex", "ohya session-start --harness claude"},
}

type sessionStartEvent struct {
	Name   string `json:"hook_event_name"`
	Source string `json:"source"`
	CWD    string `json:"cwd"`
}

type sessionStartContext struct {
	EventName         string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type sessionStartOutput struct {
	Context sessionStartContext `json:"hookSpecificOutput"`
}

func runSessionStartCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := newCommandFlags(sessionStartSpec)
	harness := flags.fs.String("harness", "", "agent harness (codex or claude)")
	if err := flags.parse(args, stdout); err != nil {
		return err
	}
	if flags.fs.NArg() != 0 {
		return flags.usageErrorf("session-start takes no positional arguments")
	}
	switch *harness {
	case "codex", "claude":
	default:
		return flags.usageErrorf("--harness must be codex or claude")
	}

	raw, err := io.ReadAll(io.LimitReader(stdin, maxSessionStartInput+1))
	if err != nil || len(raw) > maxSessionStartInput {
		return nil
	}
	var event sessionStartEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil
	}
	if event.Name != "SessionStart" || event.Source != "startup" ||
		!filepath.IsAbs(event.CWD) || strings.ContainsRune(event.CWD, 0) {
		return nil
	}
	return json.NewEncoder(stdout).Encode(sessionStartOutput{
		Context: sessionStartContext{EventName: "SessionStart", AdditionalContext: sessionStartReminder},
	})
}
