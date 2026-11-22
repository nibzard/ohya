package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var canaryStems = []string{
	"Vundrabrot", "Krellingsdorf", "Quantlyx", "Morvathene", "Zephyrind",
	"Brallokit", "Sundervik", "Trelleborn", "Harmstadt", "Ostrelling",
	"Fennmora", "Gravholte", "Ilvarune", "Jaskerland", "Lomviksen",
	"Norrekamp", "Pelhammert", "Quorindal", "Ravnskovt", "Sildveren",
	"Thornebak", "Ulvingrad", "Vestmarko", "Wyndhollow", "Yttersten",
	"Aldringar", "Bjornfjell", "Cromlechton", "Dvalinskar", "Eldrimark",
	"Fjellbron", "Gralstoken", "Hyrdvalen", "Isbrekkt", "Jorunnvik",
	"Kvitsteinn", "Lysningdal", "Malmgrenna", "Nordvikke", "Ormbekken",
	"Perlefonn", "Qvistromm", "Rimfrostvik", "Steingjord", "Tusseladd",
	"Ulvemann", "Vindrynne", "Ymerton",
}

type plantedSession struct {
	Agent  string `json:"agent"`
	Fact   string `json:"fact"`
	Prompt string `json:"prompt"`
}

type canaryPlan struct {
	Seed    int64            `json:"seed"`
	Planted []plantedSession `json:"planted"`

	code1, code3, code5, code6, code8 string
	v1, v2, v3, v4, v5Old, v5New, v7  int
	plantDay                          string
}

type plantOptions struct {
	out     string
	claude  string
	codex   string
	pi      string
	seed    int64
	home    string
	timeout time.Duration
	skip    []string
}

func plant(ctx context.Context, opts plantOptions) error {
	if err := os.MkdirAll(opts.out, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(opts.out)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("output directory %s is not empty; move it aside first", opts.out)
	}
	if _, err := os.Stat(opts.home); err != nil {
		return fmt.Errorf("-home %s: %w", opts.home, err)
	}
	skip := map[string]bool{}
	for _, a := range opts.skip {
		skip[a] = true
	}

	plan := newCanaryPlan(opts.seed)
	logPath := filepath.Join(opts.out, "plant.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logf := func(format string, args ...any) {
		fmt.Fprintf(io.MultiWriter(os.Stdout, logFile), format+"\n", args...)
	}
	logf("recall-bench plant: seed=%d home=%s", plan.Seed, opts.home)

	sessions := plan.sessionList()
	ran := 0
	for i, s := range sessions {
		if skip[s.Agent] {
			logf("skip %s fact %d of %d (agent excluded)", s.Agent, i+1, len(sessions))
			continue
		}
		if err := plantOneWithRetry(ctx, opts, s, logf, i); err != nil {
			return err
		}
		ran++
	}

	factsPath := filepath.Join(opts.out, "canary-facts.json")
	if err := writeJSON(factsPath, plan); err != nil {
		return err
	}
	qs := plan.questions()
	if err := writeJSON(canaryQuestionsPath(opts.out), qs); err != nil {
		return err
	}
	logf("planted %d of %d planned sessions over agents %s", ran, len(sessions), agentSummary(sessions))
	logf("questions: %s", canaryQuestionsPath(opts.out))
	logf("wait for the settle gate (10 minutes of session idle) and hub ingestion, then run the bench")
	return nil
}

func agentSummary(sessions []plantedSession) string {
	var kinds []string
	seen := map[string]bool{}
	for _, s := range sessions {
		if !seen[s.Agent] {
			seen[s.Agent] = true
			kinds = append(kinds, s.Agent)
		}
	}
	return strings.Join(kinds, ", ")
}

func plantOneWithRetry(ctx context.Context, opts plantOptions, s plantedSession, logf func(string, ...any), index int) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = plantOne(ctx, opts, s, logf, index)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		logf("planting %s fact %d attempt %d failed: %v", s.Agent, index+1, attempt, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(20 * time.Second):
		}
	}
	return err
}

func plantOne(ctx context.Context, opts plantOptions, s plantedSession, logf func(string, ...any), index int) error {
	runCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	var cmd *exec.Cmd
	prompt := "This is a record-keeping note for project memory. Restate the following decision in one sentence, changing nothing of its substance, then stop. Do not use any tools.\n\nDecision: " + s.Fact
	switch s.Agent {
	case "claude":
		cmd = exec.CommandContext(runCtx, opts.claude, "-p", prompt)
	case "codex":
		cmd = exec.CommandContext(runCtx, opts.codex, "exec", "--skip-git-repo-check", prompt)
	case "pi":
		cmd = exec.CommandContext(runCtx, opts.pi, "--print", "--no-tools", "--", prompt)
	default:
		return fmt.Errorf("unknown planting agent %q", s.Agent)
	}
	cmd.Dir = opts.home
	cmd.Env = plantingEnv()
	runLogDir := filepath.Join(opts.out, "plant-runs")
	if err := os.MkdirAll(runLogDir, 0o700); err != nil {
		return err
	}
	runLog, err := os.Create(filepath.Join(runLogDir, fmt.Sprintf("%02d-%s.txt", index+1, s.Agent)))
	if err != nil {
		return err
	}
	defer runLog.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, runLog, runLog
	cmd.WaitDelay = 30 * time.Second
	start := time.Now()
	err = cmd.Run()
	if runCtx.Err() != nil {
		return fmt.Errorf("planting %s run %d timed out after %s: %w", s.Agent, index+1, opts.timeout, runCtx.Err())
	}
	if err != nil {
		return fmt.Errorf("planting %s run %d failed: %w", s.Agent, index+1, err)
	}
	logf("planted %s fact %d (%.1fs)", s.Agent, index+1, time.Since(start).Seconds())
	return nil
}

func newCanaryPlan(seed int64) *canaryPlan {
	rng := rand.New(rand.NewSource(seed))
	p := &canaryPlan{Seed: seed, plantDay: time.Now().UTC().Format("2006-01-02")}
	p.code1 = newCodeword(rng)
	p.code3 = newCodeword(rng)
	p.code5 = newCodeword(rng)
	p.code6 = newCodeword(rng)
	p.code8 = newCodeword(rng)
	p.v1 = 20 + rng.Intn(80)
	p.v2 = 30 + 10*rng.Intn(16)
	p.v3 = 8 + rng.Intn(57)
	p.v4 = 200 + 10*rng.Intn(31)
	ttls := []int{5, 10, 15, 20, 30, 45, 60}
	i1, i2 := rng.Intn(len(ttls)), rng.Intn(len(ttls)-1)
	if i2 >= i1 {
		i2++
	}
	p.v5Old, p.v5New = ttls[i1], ttls[i2]
	p.v7 = 40 + 5*rng.Intn(17)
	return p
}

func newCodeword(rng *rand.Rand) string {
	return fmt.Sprintf("%s-%02d", canaryStems[rng.Intn(len(canaryStems))], 10+rng.Intn(90))
}

func plantingEnv() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TERM=dumb",
		"LANG=C.UTF-8",
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANTHROPIC_") {
			env = append(env, kv)
		}
	}
	return env
}

func (p *canaryPlan) sessionList() []plantedSession {
	return []plantedSession{
		{Agent: "claude", Fact: fmt.Sprintf("the %s retry ceiling is %d milliseconds.", p.code1, p.v1)},
		{Agent: "claude", Fact: fmt.Sprintf("the telemetry flush interval for the north sensor deck is %d seconds.", p.v2)},
		{Agent: "codex", Fact: fmt.Sprintf("the %s buffer holds %d entries before spill.", p.code3, p.v3)},
		{Agent: "pi", Fact: fmt.Sprintf("the amber relay engages at %d degrees.", p.v4)},
		{Agent: "claude", Fact: fmt.Sprintf("the %s token TTL is %d minutes.", p.code5, p.v5Old)},
		{Agent: "claude", Fact: fmt.Sprintf("Correction to an earlier decision: the %s token TTL is %d minutes; the %d-minute figure was wrong.", p.code5, p.v5New, p.v5Old)},
		{Agent: "claude", Fact: fmt.Sprintf("the %s workstream, unlike the %s latch, has no dedicated runbook.", p.code1, p.code6)},
		{Agent: "codex", Fact: fmt.Sprintf("the %s latch engages at %d newtons.", p.code6, p.v7)},
	}
}

func (p *canaryPlan) questions() []Question {
	return []Question{
		{
			Key: "c1-needle-claude", Tier: "needle",
			Prompt:           fmt.Sprintf("Find the recorded decision about the %s retry ceiling. Report the value in milliseconds.", p.code1),
			AnswerContains:   []string{fmt.Sprintf("%d milliseconds", p.v1)},
			EvidenceMarker:   fmt.Sprintf("%s retry ceiling is %d milliseconds", p.code1, p.v1),
			ExpectedSessions: 1, ExpectedFiles: 1,
		},
		{
			Key: "c2-paraphrase-claude", Tier: "paraphrase",
			Prompt:           "In an earlier session we recorded how often the north sensor deck flushes telemetry. Report that interval.",
			AnswerContains:   []string{fmt.Sprintf("%d seconds", p.v2)},
			EvidenceMarker:   fmt.Sprintf("telemetry flush interval for the north sensor deck is %d seconds", p.v2),
			ExpectedSessions: 1, ExpectedFiles: 1,
		},
		{
			Key: "c3-needle-codex", Tier: "needle",
			Prompt:           fmt.Sprintf("Find the recorded decision about the %s buffer capacity. Report the number of entries.", p.code3),
			AnswerContains:   []string{fmt.Sprintf("%d entries", p.v3)},
			EvidenceMarker:   fmt.Sprintf("%s buffer holds %d entries", p.code3, p.v3),
			ExpectedSessions: 1, ExpectedFiles: 1,
		},
		{
			Key: "c4-timeboxed-pi", Tier: "timeboxed",
			Prompt:           fmt.Sprintf("In a session recorded on %s, we set the engagement threshold of the amber relay. Report the value in degrees.", p.plantDay),
			AnswerContains:   []string{fmt.Sprintf("%d degrees", p.v4)},
			EvidenceMarker:   fmt.Sprintf("amber relay engages at %d degrees", p.v4),
			ExpectedSessions: 1, ExpectedFiles: 1,
		},
		{
			Key: "c5-correction-claude", Tier: "correction",
			Prompt:           fmt.Sprintf("A decision about the %s token TTL was recorded and later corrected. Report the TTL that stands after the correction.", p.code5),
			AnswerContains:   []string{fmt.Sprintf("%d minutes", p.v5New)},
			EvidenceMarker:   fmt.Sprintf("%s token TTL is %d minutes", p.code5, p.v5New),
			ExpectedSessions: 1, ExpectedFiles: 1,
		},
		{
			Key: "c6-aggregate", Tier: "aggregate",
			Prompt:     fmt.Sprintf("Across all recorded sessions, in how many distinct sessions does the term %q appear?", p.code1),
			CountValue: 2,
		},
		{
			Key: "c7-multihop", Tier: "multihop",
			Prompt:           fmt.Sprintf("One recorded session mentions the %s latch without its number; another session carries the number. At what force does the %s latch engage?", p.code6, p.code6),
			AnswerContains:   []string{fmt.Sprintf("%d newtons", p.v7)},
			EvidenceMarker:   fmt.Sprintf("%s latch engages at %d newtons", p.code6, p.v7),
			ExpectedSessions: 1, ExpectedFiles: 1,
		},
		{
			Key: "c8-negative", Tier: "negative", Negative: true,
			Prompt: fmt.Sprintf("Did we ever record a decision about the %s sharding plan (splitting the event store across buckets)? If it was recorded, report what was decided.", p.code8),
		},
	}
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
