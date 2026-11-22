package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	armRaw  = "raw"
	armOhya = "ohya"
)

type benchOptions struct {
	out       string
	questions []string
	ohya      string
	claude    string
	hubDB     string
	state     string
	repeats   int
	timeout   time.Duration
	roots     []string
}

type runRecord struct {
	Question        string         `json:"question"`
	Arm             string         `json:"arm"`
	Repeat          int            `json:"repeat"`
	Verdict         string         `json:"verdict"`
	WallSeconds     float64        `json:"wall_seconds"`
	Model           string         `json:"model"`
	InputTokens     int64          `json:"input_tokens"`
	OutputTokens    int64          `json:"output_tokens"`
	CacheReadTokens int64          `json:"cache_read_tokens"`
	CacheNewTokens  int64          `json:"cache_new_tokens"`
	ToolCalls       int            `json:"tool_calls"`
	ToolKinds       map[string]int `json:"tool_kinds"`
	EmptyResults    int            `json:"empty_results"`
	ErrorResults    int            `json:"error_results"`
	ToolResultBytes int64          `json:"tool_result_bytes"`
	DurationMS      int64          `json:"duration_ms"`
	NumTurns        int            `json:"num_turns"`
	CostUSD         float64        `json:"cost_usd"`
	TraceDir        string         `json:"trace_dir"`
	Problems        []string       `json:"problems,omitempty"`
}

func bench(ctx context.Context, opts benchOptions) error {
	ohya, err := resolveExecutable(opts.ohya)
	if err != nil {
		return err
	}
	claude, err := resolveExecutable(opts.claude)
	if err != nil {
		return fmt.Errorf("claude client: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if opts.hubDB == "" {
		opts.hubDB = filepath.Join(home, ".ohya", "hub.db")
	}
	if _, err := os.Stat(opts.hubDB); err != nil {
		return fmt.Errorf("hub database: %w", err)
	}
	if opts.state == "" {
		opts.state = filepath.Join(opts.out, "resolver-state")
	}
	if err := os.MkdirAll(opts.state, 0o700); err != nil {
		return err
	}

	var paths []string
	paths = append(paths, opts.questions...)
	if _, err := os.Stat(canaryQuestionsPath(opts.out)); err == nil {
		paths = append(paths, canaryQuestionsPath(opts.out))
	}
	if len(paths) == 0 {
		return fmt.Errorf("no question files: pass -questions or plant canaries into %s first", opts.out)
	}
	questions, err := loadQuestions(paths)
	if err != nil {
		return err
	}

	resolver, err := openResolver(ctx, ohya, opts.hubDB, opts.state)
	if err != nil {
		return err
	}
	defer resolver.close()

	if err := preflight(ctx, resolver, questions, opts.roots); err != nil {
		return err
	}

	if err := writeMCPConfigs(opts, ohya); err != nil {
		return err
	}

	var records []runRecord
	for rep := 1; rep <= opts.repeats; rep++ {
		for _, q := range questions {
			arms := []string{armRaw, armOhya}
			if rep%2 == 0 {
				arms = []string{armOhya, armRaw}
			}
			for _, arm := range arms {
				rec := runArm(ctx, opts, claude, resolver, q, arm, rep)
				records = append(records, rec)
				fmt.Printf("%-4s %-22s %-5s %-22s %6.1fs calls=%d in=%d out=%d\n",
					fmt.Sprintf("r%d", rep), q.Key, arm, rec.Verdict, rec.WallSeconds,
					rec.ToolCalls, rec.InputTokens, rec.OutputTokens)
				if ctx.Err() != nil {
					return fmt.Errorf("interrupted at %s %s repeat %d", q.Key, arm, rep)
				}
			}
		}
	}

	report := buildReport(records, questions, opts)
	reportPath := filepath.Join(opts.out, "report.md")
	if err := os.WriteFile(reportPath, []byte(report), 0o600); err != nil {
		return err
	}
	fmt.Printf("report: %s\n", reportPath)
	return nil
}

func writeMCPConfigs(opts benchOptions, ohya string) error {
	empty := map[string]any{"mcpServers": map[string]any{}}
	if err := writeJSON(filepath.Join(opts.out, "mcp-empty.json"), empty); err != nil {
		return err
	}
	ohyaCfg := map[string]any{
		"mcpServers": map[string]any{
			"ohya": map[string]any{
				"command": ohya,
				"args":    []string{"mcp", "--db", opts.hubDB, "--state", opts.state},
			},
		},
	}
	return writeJSON(filepath.Join(opts.out, "mcp-ohya.json"), ohyaCfg)
}

func runArm(ctx context.Context, opts benchOptions, claude string, res *resolver, q Question, arm string, rep int) runRecord {
	dir := filepath.Join(opts.out, "runs", arm, q.Key, fmt.Sprintf("r%d", rep))
	rec := runRecord{Question: q.Key, Arm: arm, Repeat: rep, TraceDir: dir, ToolKinds: map[string]int{}}
	for _, sub := range []string{"cwd", filepath.Join("home", ".claude"), "config"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			rec.Verdict = "error"
			rec.Problems = append(rec.Problems, "prepare run dir: "+err.Error())
			return rec
		}
	}
	streamPath := filepath.Join(dir, "stream.jsonl")
	stderrPath := filepath.Join(dir, "stderr.txt")
	stream, err := os.Create(streamPath)
	if err != nil {
		rec.Verdict = "error"
		rec.Problems = append(rec.Problems, err.Error())
		return rec
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		stream.Close()
		rec.Verdict = "error"
		rec.Problems = append(rec.Problems, err.Error())
		return rec
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()

	args := []string{
		"-p", armPrompt(q, arm, opts.roots),
		"--strict-mcp-config",
		"--output-format", "stream-json",
		"--verbose",
	}
	if arm == armOhya {
		args = append(args,
			"--mcp-config", filepath.Join(opts.out, "mcp-ohya.json"),
			"--allowed-tools", "mcp__ohya__sql",
		)
	} else {
		args = append(args,
			"--mcp-config", filepath.Join(opts.out, "mcp-empty.json"),
			"--allowed-tools", "Bash,Grep,Read,Glob,LS",
		)
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, claude, args...)
	cmd.Dir = filepath.Join(dir, "cwd")
	cmd.Env = claudeRunEnv(filepath.Join(dir, "home"), filepath.Join(dir, "config"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, stream, stderr
	cmd.WaitDelay = 15 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start).Seconds()
	stream.Close()
	stderr.Close()

	m := parseClaudeStream(streamPath)
	rec.WallSeconds = wall
	rec.Model = m.Model
	rec.InputTokens, rec.OutputTokens = m.InputTokens, m.OutputTokens
	rec.CacheReadTokens, rec.CacheNewTokens = m.CacheReadTokens, m.CacheNewTokens
	rec.ToolCalls = len(m.ToolCalls)
	for _, c := range m.ToolCalls {
		rec.ToolKinds[c]++
	}
	rec.EmptyResults, rec.ErrorResults = m.EmptyResults, m.ErrorResults
	rec.ToolResultBytes = m.ToolResultBytes
	rec.DurationMS, rec.NumTurns, rec.CostUSD = m.DurationMS, m.NumTurns, m.CostUSD
	rec.Problems = append(rec.Problems, m.Problems...)
	if runCtx.Err() != nil {
		rec.Verdict = "timeout"
		rec.Problems = append(rec.Problems, "run timed out after "+opts.timeout.String())
	} else if runErr != nil && !m.OK {
		rec.Verdict = "error"
		rec.Problems = append(rec.Problems, "claude exited with error: "+runErr.Error())
	} else {
		rec.Verdict = gradeRun(context.Background(), res, q, arm, m)
	}
	writeJSON(filepath.Join(dir, "meta.json"), rec)
	return rec
}

func claudeRunEnv(home, config string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TERM=dumb",
		"LANG=C.UTF-8",
		"CLAUDE_CONFIG_DIR=" + config,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANTHROPIC_") {
			env = append(env, kv)
		}
	}
	return env
}

func preflight(ctx context.Context, res *resolver, questions []Question, roots []string) error {
	var failures []string
	for _, q := range questions {
		if q.EvidenceMarker == "" {
			continue
		}
		wantSettled := q.ExpectedSettled
		if wantSettled == 0 {
			wantSettled = q.ExpectedSessions
		}
		total, err := res.countSessionsWithText(ctx, q.EvidenceMarker, false)
		if err != nil {
			return fmt.Errorf("preflight %s: %w", q.Key, err)
		}
		settled, err := res.countSessionsWithText(ctx, q.EvidenceMarker, true)
		if err != nil {
			return fmt.Errorf("preflight %s: %w", q.Key, err)
		}
		if total != q.ExpectedSessions {
			failures = append(failures, fmt.Sprintf(
				"%s: marker matches %d hub sessions, want %d (collision or ingestion lag)", q.Key, total, q.ExpectedSessions))
		}
		if settled != wantSettled {
			failures = append(failures, fmt.Sprintf(
				"%s: marker settled in %d sessions, want %d (settle gate or ingest not done)", q.Key, settled, wantSettled))
		}
		files, err := countFilesWithText(q.EvidenceMarker, roots)
		if err != nil {
			return fmt.Errorf("preflight %s: %w", q.Key, err)
		}
		if files != q.ExpectedFiles {
			failures = append(failures, fmt.Sprintf(
				"%s: marker found in %d local files, want %d", q.Key, files, q.ExpectedFiles))
		}
		fmt.Printf("preflight %-22s hub=%d settled=%d files=%d\n", q.Key, total, settled, files)
	}
	if len(failures) > 0 {
		return fmt.Errorf("preflight failed:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}

func countFilesWithText(marker string, roots []string) (int, error) {
	cmd := exec.Command("grep", append([]string{"-rlF", "--", marker}, roots...)...)
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok && len(out) == 0 {
			return 0, nil
		}
		return 0, err
	}
	return len(strings.Split(strings.TrimSpace(string(out)), "\n")), nil
}

func resolveExecutable(name string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		abs, err := filepath.Abs(name)
		if err != nil {
			return "", err
		}
		if _, err := exec.LookPath(abs); err != nil {
			return "", err
		}
		return abs, nil
	}
	return exec.LookPath(name)
}

func fileContains(path, marker string) (bool, error) {
	cmd := exec.Command("grep", "-qF", "--", marker, path)
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
