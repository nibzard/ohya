package main

import (
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nibzard/ohya/internal/fixgen"
)

const (
	rssBudgetBytes      = 16 << 20
	statusBudgetMS      = 100.0
	telemetryCeilBytes  = (24 << 20) + (16 << 20) + (256 << 10)
	wallOverheadBudget  = 3.0
	maxRecordBytesLimit = 1 << 20
	oversizeRecordBytes = 2 << 20
	importTimeout       = 240 * time.Second
)

type runResult struct {
	rep        int
	side       string
	importMS   int64
	steadyKiB  int64
	idleCPU    float64
	idleWrKiB  int64
	obsBytes   int64
	obsFiles   int64
	statusP95  float64
	versionP95 float64
	exitCode   int
	events     int
	stopLogs   int
	err        string
}

type options struct {
	bin        string
	corpusMiB  int
	reps       int
	idle       time.Duration
	interval   time.Duration
	statusRuns int
	out        string
}

func main() {
	opts := options{}
	flag.StringVar(&opts.bin, "bin", "bin/ohya", "ohya binary to run")
	flag.IntVar(&opts.corpusMiB, "corpus-mib", 24, "clean claude-code corpus size in MiB")
	flag.IntVar(&opts.reps, "reps", 6, "paired repetitions per side")
	flag.DurationVar(&opts.idle, "idle", 75*time.Second, "steady-state window after import completes")
	flag.DurationVar(&opts.interval, "interval", time.Second, "daemon poll interval")
	flag.IntVar(&opts.statusRuns, "status-runs", 20, "status/version invocations for the read-latency p95")
	flag.StringVar(&opts.out, "out", "", "also write the markdown report to this path")
	flag.Parse()

	report, err := runPaired(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "observeoverhead:", err)
		os.Exit(1)
	}
	fmt.Print(report)
	if opts.out != "" {
		if err := os.WriteFile(opts.out, []byte(report), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "observeoverhead: write report:", err)
			os.Exit(1)
		}
	}
}

func runPaired(opts options) (string, error) {
	bin, err := filepath.Abs(opts.bin)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("binary %s: %w", bin, err)
	}
	root, err := workRoot()
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(root)
	registerCleanup(root)

	corpusDir := filepath.Join(root, "corpus")
	_, err = fixgen.Write(corpusDir, fixgen.Corpus{
		Sessions:            opts.corpusMiB / 3,
		MainMeanBytes:       2 << 20,
		SubagentsPerSession: 2,
		Seed:                7,
	})
	if err != nil {
		return "", fmt.Errorf("generate corpus: %w", err)
	}
	stopDir := filepath.Join(corpusDir, "-stop-fixture-project")
	stopRecords, err := writeStopFixtures(stopDir)
	if err != nil {
		return "", err
	}
	records, err := countRecords(corpusDir, stopDir)
	if err != nil {
		return "", err
	}
	expected := records + stopRecords

	var results []runResult
	for rep := 1; rep <= opts.reps; rep++ {
		sides := []string{"off", "on"}
		if rep%2 == 1 {
			sides = []string{"on", "off"}
		}
		for _, side := range sides {
			res, err := runOnce(opts, bin, root, rep, side, corpusDir, expected)
			if err != nil {
				return "", fmt.Errorf("rep %d side %s: %w", rep, side, err)
			}
			results = append(results, res)
			fmt.Printf("rep %d side %-3s import %6.2fs steadyRSS %5.1f MiB idleWrites %6.1f KiB observe %4d KiB statusP95 %6.1f ms exit %d\n",
				res.rep, res.side, float64(res.importMS)/1e3, float64(res.steadyKiB)/1024,
				float64(res.idleWrKiB)/1, res.obsBytes>>10, res.statusP95, res.exitCode)
		}
	}
	return renderReport(opts, bin, expected, results)
}

func runOnce(opts options, bin, root string, rep int, side, corpusDir string, expected int) (runResult, error) {
	dir := filepath.Join(root, fmt.Sprintf("rep%02d-%s", rep, side))
	dbPath := filepath.Join(dir, "hub", "hub.db")
	stateDir := filepath.Join(dir, "state")
	for _, d := range []string{filepath.Dir(dbPath), stateDir, filepath.Join(dir, "home")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return runResult{}, err
		}
	}
	res := runResult{rep: rep, side: side}

	args := []string{"ingest",
		"--db", dbPath,
		"--state", stateDir,
		"--claude-root", corpusDir, "--codex-root", "", "--pi-root", "", "--antigravity-root", "",
		"--interval", opts.interval.String(),
		"--max-record-bytes", strconv.Itoa(maxRecordBytesLimit),
		"--observe", side}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(dir, "home"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return res, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	counts, err := pollEvents(dbPath, expected, importTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		<-done
		return res, fmt.Errorf("%w; daemon stderr:\n%s", err, stderr.String())
	}
	res.importMS = time.Since(start).Milliseconds()
	res.events = counts
	observeDir := filepath.Join(stateDir, "observe")

	idleStart := procSnapshot(cmd.Process.Pid)
	writesAtStart := idleStart.writeBytes
	deadline := time.Now().Add(opts.idle)
	var steady int64
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if rss := procRSSKiB(cmd.Process.Pid); rss > steady {
			steady = rss
		}
	}
	idleEnd := procSnapshot(cmd.Process.Pid)
	res.steadyKiB = steady
	res.idleCPU = idleEnd.cpuSec - idleStart.cpuSec
	if delta := idleEnd.writeBytes - writesAtStart; delta >= 0 {
		res.idleWrKiB = int64(delta) / 1024
	}

	if side == "on" {
		home := filepath.Join(dir, "home")
		res.statusP95, err = cliP95(bin, home, opts.statusRuns, "status", "--json", "--state", stateDir)
		if err != nil {
			return res, err
		}
		res.versionP95, err = cliP95(bin, home, opts.statusRuns, "version")
		if err != nil {
			return res, err
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return res, err
	}
	select {
	case err := <-done:
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				res.exitCode = ee.ExitCode()
			} else {
				res.err = err.Error()
			}
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		res.err = "daemon ignored SIGTERM"
	}

	if counts, _, err := readCounts(dbPath); err == nil {
		res.events = counts
	}
	if res.events != expected {
		res.err = fmt.Sprintf("events %d after stop, want %d", res.events, expected)
	}
	res.obsBytes, res.obsFiles = treeBytes(observeDir), treeFiles(observeDir)
	if side == "on" {
		res.stopLogs = countStopEvents(observeDir)
		if res.stopLogs == 0 {
			res.err = strings.Join(filterNonEmpty(res.err, "no source_stopped entry in events.jsonl"), "; ")
		}
	} else if res.obsFiles != 0 {
		res.err = strings.Join(filterNonEmpty(res.err, "observation off wrote telemetry files"), "; ")
	}
	return res, nil
}

func filterNonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func countRecords(root, exclude string) (int, error) {
	total := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == exclude {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		total += strings.Count(string(data), "\n")
		return nil
	})
	return total, err
}

func pollEvents(dbPath string, expected int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		counts, _, err := readCounts(dbPath)
		if err == nil {
			if counts >= expected {
				return counts, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	counts, sessions, err := readCounts(dbPath)
	if err != nil {
		return 0, fmt.Errorf("import never completed: %w", err)
	}
	return counts, fmt.Errorf("import reached %d events (%d sessions) in %s, want %d",
		counts, sessions, timeout, expected)
}

func readCounts(dbPath string) (events, sessions int, err error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	if err := db.QueryRow("SELECT count(*) FROM events").Scan(&events); err != nil {
		return 0, 0, err
	}
	if err := db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions); err != nil {
		return 0, 0, err
	}
	return events, sessions, nil
}

func writeStopFixtures(dir string) (int, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	valid := func(id, text string) []byte {
		return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/stop-fixture","message":{"role":"user","content":%q},"timestamp":"2026-01-02T00:00:00.000Z"}`+"\n", id, text))
	}
	malformed := append([]byte{}, valid("stopfix-malformed", "record one")...)
	malformed = append(malformed, valid("stopfix-malformed", "record two")...)
	malformed = append(malformed, []byte("{\"type\":\"user\",\"sessionId\":\"stopfix-malformed\", NOT JSON\n")...)
	malformed = append(malformed, valid("stopfix-malformed", "record after the malformed line, never ingested")...)
	if err := os.WriteFile(filepath.Join(dir, "malformed.jsonl"), malformed, 0o600); err != nil {
		return 0, err
	}

	oversize := append([]byte{}, valid("stopfix-oversize", "record one")...)
	padding := bytes.Repeat([]byte{'x'}, oversizeRecordBytes)
	oversize = append(oversize, []byte(fmt.Sprintf("{\"type\":\"user\",\"sessionId\":\"stopfix-oversize\",\"message\":{\"role\":\"user\",\"content\":\"%s\"},\"timestamp\":\"2026-01-02T00:00:01.000Z\"}\n", padding))...)
	oversize = append(oversize, valid("stopfix-oversize", "record after the oversized line, never ingested")...)
	if err := os.WriteFile(filepath.Join(dir, "oversize.jsonl"), oversize, 0o600); err != nil {
		return 0, err
	}
	return 2 + 1, nil
}

func cliP95(bin, home string, runs int, args ...string) (float64, error) {
	allowed := map[int]bool{0: true}
	if len(args) > 0 && args[0] == "status" {
		allowed[3] = true
	}
	durs := make([]float64, 0, runs)
	for i := 0; i < runs; i++ {
		start := time.Now()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdout = nil
		if err := cmd.Run(); err != nil {
			code := 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			if !allowed[code] {
				return 0, fmt.Errorf("%s: %w", args[0], err)
			}
		}
		durs = append(durs, float64(time.Since(start).Microseconds())/1e3)
	}
	sort.Float64s(durs)
	return percentile(durs, 95), nil
}

func percentile(sorted []float64, pct int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (pct*len(sorted) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	return sorted[idx-1]
}

func renderReport(opts options, bin string, expected int, results []runResult) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Instrumentation overhead run (perf/observeoverhead)\n\n")
	fmt.Fprintf(&b, "binary: `%s`\n\ncorpus: %d MiB claude-code fixture (fixgen seed 7) plus two stop fixtures; reps: %d; idle window: %s; poll interval: %s; expected events: %d\n\n",
		bin, opts.corpusMiB, opts.reps, opts.idle, opts.interval, expected)
	fmt.Fprintf(&b, "## Samples\n\n")
	fmt.Fprintf(&b, "| rep | side | import s | steady RSS MiB | idle CPU s | idle writes KiB | telemetry KiB | files | status p95 ms | version p95 ms | events | stop logs | exit | note |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %d | %s | %.2f | %.1f | %.2f | %d | %d | %d | %.1f | %.1f | %d | %d | %d | %s |\n",
			r.rep, r.side, float64(r.importMS)/1e3, float64(r.steadyKiB)/1024, r.idleCPU,
			r.idleWrKiB, r.obsBytes>>10, r.obsFiles, r.statusP95, r.versionP95, r.events, r.stopLogs, r.exitCode, r.err)
	}

	byRepSide := map[string]runResult{}
	for _, r := range results {
		byRepSide[fmt.Sprintf("%d/%s", r.rep, r.side)] = r
	}
	var wallDeltas, rssDeltas []float64
	fmt.Fprintf(&b, "\n## Paired deltas\n\n| rep | import overhead %% | added steady RSS MiB |\n|---|---|---|\n")
	for rep := 1; rep <= opts.reps; rep++ {
		on := byRepSide[fmt.Sprintf("%d/on", rep)]
		off := byRepSide[fmt.Sprintf("%d/off", rep)]
		if on.importMS == 0 || off.importMS == 0 {
			continue
		}
		wall := (float64(on.importMS)/float64(off.importMS) - 1) * 100
		rss := float64(on.steadyKiB-off.steadyKiB) / 1024
		wallDeltas = append(wallDeltas, wall)
		rssDeltas = append(rssDeltas, rss)
		fmt.Fprintf(&b, "| %d | %+.1f | %+.1f |\n", rep, wall, rss)
	}

	medianWall, medianRSS := percentile(float64s(wallDeltas), 50), percentile(float64s(rssDeltas), 50)

	var statusP95 []float64
	maxTelemetry := int64(0)
	for _, r := range results {
		if r.side == "on" && r.statusP95 > 0 {
			statusP95 = append(statusP95, r.statusP95)
		}
		if r.obsBytes > maxTelemetry {
			maxTelemetry = r.obsBytes
		}
	}
	sort.Float64s(statusP95)
	p95Status := percentile(statusP95, 95)

	fmt.Fprintf(&b, "\n## Budget verdicts (plans/001 'Cost and retention budgets')\n\n")
	fmt.Fprintf(&b, "- median end-to-end import wall overhead (observe on vs off): %+.1f%%; budget <= %.0f%%: %s\n",
		medianWall, wallOverheadBudget, verdict(medianWall <= wallOverheadBudget))
	fmt.Fprintf(&b, "- median added steady RSS per daemon: %+.1f MiB; budget <= %.0f MiB: %s\n",
		medianRSS, float64(rssBudgetBytes>>20), verdict(medianRSS <= float64(rssBudgetBytes>>20)))
	fmt.Fprintf(&b, "- `status --json` CLI end-to-end p95 across on-runs: %.1f ms; budget <= %.0f ms (cached read, process startup excluded): %s (the CLI figure includes startup; subtract the `ohya version` baseline in the samples table for the read itself)\n",
		p95Status, statusBudgetMS, verdict(p95Status <= statusBudgetMS))
	fmt.Fprintf(&b, "- retained telemetry per instance after a run: at most %d KiB across %d files; ceiling 24 MiB logs + 16 MiB history + 256 KiB snapshot: %s\n",
		maxTelemetry>>10, countMaxFiles(results), verdict(maxTelemetry <= telemetryCeilBytes))
	wallOK := medianWall <= wallOverheadBudget
	rssOK := medianRSS <= float64(rssBudgetBytes>>20)
	statusOK := p95Status <= statusBudgetMS
	if !wallOK || !rssOK || !statusOK || maxTelemetry > telemetryCeilBytes {
		fmt.Fprintf(&b, "\nA budget was exceeded: treat it as an investigation trigger, not a silently raised target (plans/001).\n")
	}
	return b.String(), nil
}

func countMaxFiles(results []runResult) int64 {
	var max int64
	for _, r := range results {
		if r.obsFiles > max {
			max = r.obsFiles
		}
	}
	return max
}

func float64s(in []float64) []float64 {
	out := append([]float64(nil), in...)
	sort.Float64s(out)
	return out
}

func verdict(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

type procSample struct {
	writeBytes int64
	cpuSec     float64
}

func procSnapshot(pid int) procSample {
	rss, cpu := procResources(pid)
	_ = rss
	return procSample{writeBytes: procIOField(pid, "write_bytes"), cpuSec: cpu}
}

func procRSSKiB(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				n, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func procResources(pid int) (rssKiB int64, cpuSec float64) {
	rssKiB = procRSSKiB(pid)
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return rssKiB, 0
	}
	if idx := strings.LastIndexByte(string(data), ')'); idx >= 0 && idx+2 <= len(data) {
		fields := strings.Fields(string(data[idx+2:]))
		if len(fields) >= 13 {
			utime, _ := strconv.ParseFloat(fields[11], 64)
			stime, _ := strconv.ParseFloat(fields[12], 64)
			cpuSec = (utime + stime) / 100
		}
	}
	return rssKiB, cpuSec
}

func procIOField(pid int, name string) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+":") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				n, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func treeBytes(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func treeFiles(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			total++
		}
		return nil
	})
	return total
}

func countStopEvents(observeDir string) int {
	var total int
	filepath.WalkDir(observeDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "events.jsonl" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		total += strings.Count(string(data), `"event_code":"source_stopped"`)
		return nil
	})
	return total
}

func workRoot() (string, error) {
	prefix := os.Getenv("LOOPER_RESOURCE_PREFIX")
	if prefix == "" {
		prefix = "ohya"
	}
	return os.MkdirTemp("", prefix+"-observeoverhead-")
}

func registerCleanup(root string) {
	path := os.Getenv("LOOPER_RESOURCE_CLEANUP_FILE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "test -e %s && rm -rf -- %s\n", shellQuote(root), shellQuote(root))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
