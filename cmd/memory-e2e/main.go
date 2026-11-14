package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "memory-e2e:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("memory-e2e", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "memory-e2e",
		"output `directory` holding the isolated database, client state, and traces; must be empty or absent")
	ohyaFlag := fs.String("ohya", "bin/ohya", "`path` to the ohya binary under test")
	claudeFlag := fs.String("claude", "claude", "`command` of the real Claude Code client")
	codexFlag := fs.String("codex", "codex", "`command` of the real Codex client")
	timeout := fs.Duration("timeout", 5*time.Minute, "per-case `timeout` for one agent run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if err := checkCredentials(); err != nil {
		return err
	}
	ohya, err := resolveExecutable(*ohyaFlag)
	if err != nil {
		return err
	}
	claude, err := resolveExecutable(*claudeFlag)
	if err != nil {
		return fmt.Errorf("claude client: %w", err)
	}
	codex, err := resolveExecutable(*codexFlag)
	if err != nil {
		return fmt.Errorf("codex client: %w", err)
	}
	if *timeout <= 0 {
		return errors.New("-timeout must be positive")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, err := newGate(*out, ohya, claude, codex, *timeout)
	if err != nil {
		return err
	}
	defer g.close()

	g.log.line("memory-e2e: ohya=%s claude=%s codex=%s timeout=%s", g.ohya, g.claude, g.codex, g.timeout)
	g.log.line("output directory: %s (all state and traces stay here)", g.out)

	if err := g.prepare(ctx); err != nil {
		return err
	}
	if err := g.seedAndVerify(ctx); err != nil {
		return err
	}
	defer g.resolver.close()

	failed := 0
	for _, client := range []struct {
		name string
		fn   func(context.Context, caseDef) *runResult
	}{
		{"claude-code", g.runClaudeCase},
		{"codex", g.runCodexCase},
	} {
		for _, c := range gateCases {
			caseCtx, cancel := context.WithTimeout(ctx, g.timeout)
			start := time.Now()
			result := client.fn(caseCtx, c)
			cancel()
			problems := checkCase(c, result, g.resolver.eventText)
			if len(problems) == 0 {
				g.log.line("PASS %-12s %-17s (%.1fs, tool calls: %s)",
					client.name, c.key, time.Since(start).Seconds(), callsOrNone(result.toolCalls))
				continue
			}
			failed++
			g.log.line("FAIL %-12s %-17s (%.1fs)", client.name, c.key, time.Since(start).Seconds())
			for _, p := range problems {
				g.log.line("     %s", p)
			}
			g.log.line("     answer: %s", oneLine(result.answer))
			g.log.line("     traces: %s", result.traceDir)
		}
	}

	total := 2 * len(gateCases)
	g.log.line("memory-e2e: %d/%d cases passed", total-failed, total)
	if failed > 0 {
		return fmt.Errorf("%d of %d cases failed; see %s", failed, total, g.out)
	}
	return nil
}

type gate struct {
	out     string
	ohya    string
	claude  string
	codex   string
	timeout time.Duration

	dbPath       string
	stateDir     string
	claudeRoot   string
	codexRoot    string
	claudeConfig string
	claudeHome   string
	codexHome    string
	tracesDir    string

	log      report
	resolver *resolver
	close    func()
}

type report struct{ w io.Writer }

func (r report) line(format string, args ...any) {
	fmt.Fprintf(r.w, format+"\n", args...)
}

func newGate(out, ohya, claude, codex string, timeout time.Duration) (*gate, error) {
	if err := os.MkdirAll(out, 0o700); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		return nil, fmt.Errorf("output directory %s is not empty; move it aside first", out)
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	reportFile, err := os.Create(filepath.Join(out, "report.txt"))
	if err != nil {
		return nil, err
	}
	g := &gate{
		out:          abs,
		ohya:         ohya,
		claude:       claude,
		codex:        codex,
		timeout:      timeout,
		dbPath:       filepath.Join(abs, "hub", "hub.db"),
		stateDir:     filepath.Join(abs, "state"),
		claudeRoot:   filepath.Join(abs, "claude-projects"),
		codexRoot:    filepath.Join(abs, "codex-sessions"),
		claudeConfig: filepath.Join(abs, "claude", "config"),
		claudeHome:   filepath.Join(abs, "claude", "home"),
		codexHome:    filepath.Join(abs, "codex", "home"),
		tracesDir:    filepath.Join(abs, "traces"),
		log:          report{w: io.MultiWriter(os.Stdout, reportFile)},
		close:        func() { reportFile.Close() },
	}
	for _, dir := range []string{
		filepath.Dir(g.dbPath), g.stateDir, g.claudeRoot, g.claudeConfig, g.claudeHome,
		filepath.Join(g.codexHome, "sessions"), g.tracesDir,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return g, nil
}

func (g *gate) prepare(ctx context.Context) error {
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"ohya": map[string]any{
				"command": g.ohya,
				"args":    []string{"mcp", "--db", g.dbPath, "--state", g.stateDir},
			},
		},
	}
	if err := writeJSON(filepath.Join(g.out, "claude", "mcp.json"), mcpConfig); err != nil {
		return err
	}
	toml := fmt.Sprintf("[mcp_servers.ohya]\ncommand = %q\nargs = [%q, %q, %q, %q, %q]\n",
		g.ohya, "mcp", "--db", g.dbPath, "--state", g.stateDir)
	if err := os.WriteFile(filepath.Join(g.codexHome, "config.toml"), []byte(toml), 0o600); err != nil {
		return err
	}
	if err := g.codexLogin(ctx); err != nil {
		return err
	}
	return g.verifyCodexServer(ctx)
}

func (g *gate) codexLogin(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, g.codex, "login", "--with-api-key")
	cmd.Env = g.codexEnv()
	cmd.Stdin = strings.NewReader(os.Getenv("OPENAI_API_KEY"))
	logPath := filepath.Join(g.tracesDir, "codex-login.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex login inside %s failed (see %s): %w", g.codexHome, logPath, err)
	}
	return nil
}

func (g *gate) verifyCodexServer(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, g.codex, "mcp", "list")
	cmd.Env = g.codexEnv()
	cmd.WaitDelay = 30 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex mcp list: %w: %s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "ohya ") {
			return nil
		}
	}
	return fmt.Errorf("codex mcp list does not show the ohya server (config: %s, output: %s)",
		filepath.Join(g.codexHome, "config.toml"), out)
}

func (g *gate) seedAndVerify(ctx context.Context) error {
	if err := writeSeedFixtures(g.claudeRoot, g.codexRoot); err != nil {
		return err
	}
	ingestLog := filepath.Join(g.tracesDir, "ingest.log")
	logFile, err := os.Create(ingestLog)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.CommandContext(ctx, g.ohya, "ingest", "--once",
		"--db", g.dbPath, "--state", g.stateDir,
		"--claude-root", g.claudeRoot, "--codex-root", g.codexRoot, "--pi-root", "")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ohya ingest failed (see %s): %w", ingestLog, err)
	}

	res, err := openResolver(ctx, g.ohya, g.dbPath, g.stateDir)
	if err != nil {
		return err
	}
	g.resolver = res
	for _, marker := range seedMarkers {
		n, err := res.countEventsWithText(ctx, marker)
		if err != nil {
			return fmt.Errorf("verify seed marker %q: %w", marker, err)
		}
		if n != 1 {
			return fmt.Errorf("seed marker %q matched %d events, want 1 (see %s)", marker, n, ingestLog)
		}
		g.log.line("seeded: %q -> exactly one event", marker)
	}
	return nil
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

func checkCredentials() error {
	var missing []string
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
		missing = append(missing, "ANTHROPIC_AUTH_TOKEN or ANTHROPIC_API_KEY (Claude Code headless auth)")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		missing = append(missing, "OPENAI_API_KEY (Codex `codex login --with-api-key`)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing ambient credentials for the real agent clients: %s",
			strings.Join(missing, "; "))
	}
	return nil
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 200 {
		s = s[:200] + " …"
	}
	if s == "" {
		return "(no answer)"
	}
	return s
}

func callsOrNone(calls []string) string {
	if len(calls) == 0 {
		return "none"
	}
	return strings.Join(calls, ", ")
}
