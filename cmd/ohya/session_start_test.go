package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

func TestSessionStartReminder(t *testing.T) {
	const startup = `{"hook_event_name":"SessionStart","source":"startup","cwd":"/workspace/marker","extra":{"session_id":"marker"}}`
	const want = `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"When the user asks to continue prior work, recover a handoff, or revisit a past decision, use Ohya to search this project's history. Verify findings against the current checkout before acting."}}` + "\n"

	for _, harness := range []string{"codex", "claude"} {
		t.Run(harness, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := runSessionStartCommand([]string{"--harness", harness}, strings.NewReader(startup), &stdout); err != nil {
				t.Fatal(err)
			}
			if got := stdout.String(); got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
		})
	}
}

func TestSessionStartIgnoresInapplicableInput(t *testing.T) {
	const valid = `{"hook_event_name":"SessionStart","source":"startup","cwd":"/workspace"}`
	cases := map[string]string{
		"empty":         "",
		"malformed":     "{",
		"second object": valid + "{}",
		"oversized":     valid + strings.Repeat(" ", maxSessionStartInput),
		"array":         "[" + valid + "]",
		"null":          "null",
		"other event":   strings.Replace(valid, "SessionStart", "Stop", 1),
		"resume":        strings.Replace(valid, "startup", "resume", 1),
		"fork":          strings.Replace(valid, "startup", "fork", 1),
		"compact":       strings.Replace(valid, "startup", "compact", 1),
		"clear":         strings.Replace(valid, "startup", "clear", 1),
		"relative cwd":  strings.Replace(valid, "/workspace", "workspace", 1),
		"empty cwd":     strings.Replace(valid, "/workspace", "", 1),
		"invalid cwd":   strings.Replace(valid, "/workspace", `/workspace\u0000`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := runSessionStartCommand([]string{"--harness", "codex"}, strings.NewReader(input), &stdout); err != nil {
				t.Fatal(err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", &stdout)
			}
		})
	}
}

func TestSessionStartReadFailure(t *testing.T) {
	var stdout bytes.Buffer
	input := iotest.ErrReader(errors.New("read failed"))
	if err := runSessionStartCommand([]string{"--harness", "codex"}, input, &stdout); err != nil || stdout.Len() != 0 {
		t.Fatalf("stdout = %q, error = %v, want empty stdout and no error", &stdout, err)
	}
}

func TestSessionStartCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cliMain([]string{"session-start", "--help"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), sessionStartSpec.usageLine) {
		t.Fatalf("help exit = %d, stdout = %q, stderr = %q", code, &stdout, &stderr)
	}
	stdout.Reset()
	if code := cliMain([]string{"session-start", "--harness", "unsupported"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--harness must be codex or claude") {
		t.Fatalf("usage error exit = %d, stdout = %q, stderr = %q", code, &stdout, &stderr)
	}
}
