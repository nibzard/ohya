package main

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/observe"
)

const freshnessSessionID = "00000000-0000-4000-8000-0000000000a1"
const backlogSessionID = "00000000-0000-4000-8000-0000000000b2"

type freshnessOptions struct {
	samples     int
	appendEvery time.Duration
	pollEvery   time.Duration
	timeout     time.Duration
	uploadEvery time.Duration
	seed        uint64
	observe     string

	backlogBPS    int
	queriesEvery  time.Duration
	queryTimeout  time.Duration
	devices       int
	deviceMiB     int
	pairLead      time.Duration
	idleSampleFor time.Duration
}

func freshnessObserveEnv(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("OHYA_FRESH_OBSERVE")
	if raw == "" {
		return "on"
	}
	if raw != "on" && raw != "off" {
		t.Fatalf("OHYA_FRESH_OBSERVE = %q: want on or off", raw)
	}
	return raw
}

func freshnessObserveArgs(mode string) []string {
	return []string{"--observe", mode}
}

type freshnessSample struct {
	marker        string
	appendedAt    time.Time
	lag           time.Duration
	queryDur      time.Duration
	visible       bool
	timedOut      bool
	overdue       bool
	lateDelivered bool
}

type loadedQueryStats struct {
	name     string
	durs     []time.Duration
	errors   int
	timeouts int
}

type freshnessReport struct {
	run            string
	opts           freshnessOptions
	phaseOffset    time.Duration
	warmupQuery    time.Duration
	samples        []freshnessSample
	queryDurs      []time.Duration
	queries        int
	queryErrors    int
	deliveredLate  int
	neverDelivered int
	hubEvents      int

	backlogWritten int64
	backlogAcked   int64
	deviceProgress []deviceProgressLine
	loadedQueries  []loadedQueryStats
	loadWindow     time.Duration
	queryErrTexts  []string
}

type deviceProgressLine struct {
	deviceID   string
	fileBytes  int64
	ackedBytes int64
}

type extraDevice struct {
	stateDir   string
	claudeRoot string
	code       string
	cmd        *exec.Cmd
	log        *bytes.Buffer
	deviceID   string
}

type freshnessStack struct {
	hubDB        string
	hubState     string
	devState     string
	sessionPath  string
	backlogPath  string
	observe      string
	serveCmd     *exec.Cmd
	serveLog     *bytes.Buffer
	joinCmd      *exec.Cmd
	joinLog      *bytes.Buffer
	extraDevices []*extraDevice
	sampler      *resourceSampler
	session      *mcp.ClientSession
	mcpCtx       context.Context
	mcpCancel    context.CancelFunc
}

func freshnessIntEnv(t *testing.T, name string, def int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		t.Fatalf("%s = %q: want a non-negative integer", name, raw)
	}
	return n
}

func freshnessDurationEnv(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		t.Fatalf("%s = %q: want a positive duration", name, raw)
	}
	return d
}

func freshnessSeedEnv(t *testing.T) uint64 {
	t.Helper()
	raw := os.Getenv("OHYA_FRESH_SEED")
	if raw == "" {
		var b [8]byte
		if _, err := cryptorand.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		return binary.LittleEndian.Uint64(b[:])
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		t.Fatalf("OHYA_FRESH_SEED = %q: want an unsigned integer", raw)
	}
	return n
}

func freshnessOptionsFromEnv(t *testing.T) freshnessOptions {
	t.Helper()
	return freshnessOptions{
		samples:     freshnessIntEnv(t, "OHYA_FRESH_SAMPLES", 100),
		appendEvery: freshnessDurationEnv(t, "OHYA_FRESH_APPEND_EVERY", 200*time.Millisecond),
		pollEvery:   freshnessDurationEnv(t, "OHYA_FRESH_POLL_EVERY", 100*time.Millisecond),
		timeout:     freshnessDurationEnv(t, "OHYA_FRESH_TIMEOUT", 30*time.Second),
		uploadEvery: freshnessDurationEnv(t, "OHYA_FRESH_UPLOAD_EVERY", 2*time.Second),
		seed:        freshnessSeedEnv(t),
		observe:     freshnessObserveEnv(t),

		backlogBPS:    freshnessIntEnv(t, "OHYA_FRESH_BACKLOG_BPS", 0),
		queriesEvery:  freshnessDurationEnv(t, "OHYA_FRESH_QUERIES_EVERY", 0),
		queryTimeout:  freshnessDurationEnv(t, "OHYA_FRESH_QUERY_TIMEOUT", 5*time.Second),
		devices:       freshnessIntEnv(t, "OHYA_FRESH_DEVICES", 1),
		deviceMiB:     freshnessIntEnv(t, "OHYA_FRESH_DEVICE_MIB", 96),
		pairLead:      freshnessDurationEnv(t, "OHYA_FRESH_PAIR_LEAD", 30*time.Second),
		idleSampleFor: freshnessDurationEnv(t, "OHYA_FRESH_IDLE", 5*time.Second),
	}
}

type resourceSampler struct {
	hubDB string
	every time.Duration

	mu        sync.Mutex
	tracked   map[int]string
	peakRSSKB map[int]int64
	cpuSec    map[int]float64
	samples   int

	idleOpen     bool
	idleCPUBegin float64
	idleCPUEnd   float64
	idleRSSKB    int64
	idleRSSMaxKB int64
	idleCount    int
	idleWindow   time.Duration

	dbBytes int64
	walPeak int64

	stop    chan struct{}
	stopped chan struct{}
}

func startResourceSampler(hubDB string, every time.Duration) *resourceSampler {
	s := &resourceSampler{
		hubDB:     hubDB,
		every:     every,
		tracked:   map[int]string{},
		peakRSSKB: map[int]int64{},
		cpuSec:    map[int]float64{},
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *resourceSampler) track(name string, cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tracked[cmd.Process.Pid] = name
}

func (s *resourceSampler) beginIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idleOpen = true
	s.idleRSSKB, s.idleRSSMaxKB, s.idleCount = 0, 0, 0
	for pid := range s.tracked {
		if s.tracked[pid] != "serve" {
			continue
		}
		if _, cpu, ok := procResources(pid); ok {
			s.idleCPUBegin = cpu
		}
	}
}

func (s *resourceSampler) endIdle(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.idleOpen {
		return
	}
	s.idleOpen = false
	s.idleWindow = d
	for pid := range s.tracked {
		if s.tracked[pid] != "serve" {
			continue
		}
		if _, cpu, ok := procResources(pid); ok {
			s.idleCPUEnd = cpu
		}
	}
}

func (s *resourceSampler) loop() {
	defer close(s.stopped)
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		s.mu.Lock()
		pids := make([]int, 0, len(s.tracked))
		for pid := range s.tracked {
			pids = append(pids, pid)
		}
		for _, pid := range pids {
			rss, cpu, ok := procResources(pid)
			if !ok {
				continue
			}
			s.samples++
			if rss > s.peakRSSKB[pid] {
				s.peakRSSKB[pid] = rss
			}
			s.cpuSec[pid] = cpu
			if s.idleOpen && s.tracked[pid] == "serve" {
				s.idleRSSKB += rss
				s.idleCount++
				if rss > s.idleRSSMaxKB {
					s.idleRSSMaxKB = rss
				}
			}
		}
		s.mu.Unlock()
		if st, err := os.Stat(s.hubDB); err == nil {
			s.mu.Lock()
			s.dbBytes = st.Size()
			s.mu.Unlock()
		}
		if st, err := os.Stat(s.hubDB + "-wal"); err == nil {
			s.mu.Lock()
			if st.Size() > s.walPeak {
				s.walPeak = st.Size()
			}
			s.mu.Unlock()
		}
	}
}

func (s *resourceSampler) stopAndWait() {
	close(s.stop)
	<-s.stopped
}

type procPeakLine struct {
	name      string
	peakRSSKB int64
	cpuSec    float64
}

func (s *resourceSampler) peaks() (procs []procPeakLine, dbBytes, walPeak int64, samples int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pid, name := range s.tracked {
		procs = append(procs, procPeakLine{name: name, peakRSSKB: s.peakRSSKB[pid], cpuSec: s.cpuSec[pid]})
	}
	sort.Slice(procs, func(a, b int) bool {
		if procs[a].name == procs[b].name {
			return procs[a].peakRSSKB < procs[b].peakRSSKB
		}
		if procs[a].name == "serve" {
			return true
		}
		if procs[b].name == "serve" {
			return false
		}
		return procs[a].name < procs[b].name
	})
	return procs, s.dbBytes, s.walPeak, s.samples
}

func (s *resourceSampler) idleStats() (meanRSSKB, maxRSSKB int64, cpuSec float64, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleCount > 0 {
		meanRSSKB = s.idleRSSKB / int64(s.idleCount)
	}
	return meanRSSKB, s.idleRSSMaxKB, s.idleCPUEnd - s.idleCPUBegin, s.idleWindow
}

func procResources(pid int) (rssKB int64, cpuSec float64, ok bool) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, 0, false
			}
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			rssKB = n
			break
		}
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, false
	}
	if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 <= len(stat) {
		fields := strings.Fields(string(stat[i+2:]))
		if len(fields) >= 13 {
			utime, err1 := strconv.ParseFloat(fields[11], 64)
			stime, err2 := strconv.ParseFloat(fields[12], 64)
			if err1 == nil && err2 == nil {
				cpuSec = (utime + stime) / 100.0
			}
		}
	}
	return rssKB, cpuSec, true
}

func startFreshnessStack(t *testing.T, opts freshnessOptions) *freshnessStack {
	t.Helper()
	dir := transportTestDir(t)
	st := &freshnessStack{
		hubState:    filepath.Join(dir, "hub-state"),
		hubDB:       filepath.Join(dir, "hub", "hub.db"),
		devState:    filepath.Join(dir, "dev-state"),
		sessionPath: filepath.Join(dir, "claude-projects", "session.jsonl"),
		backlogPath: filepath.Join(dir, "claude-projects", "zbacklog.jsonl"),
		observe:     opts.observe,
		serveLog:    &bytes.Buffer{},
		joinLog:     &bytes.Buffer{},
	}
	if err := os.MkdirAll(filepath.Dir(st.sessionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.sessionPath, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.backlogPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < opts.devices; i++ {
		st.extraDevices = append(st.extraDevices, &extraDevice{
			stateDir:   filepath.Join(dir, fmt.Sprintf("dev%d-state", i+1)),
			claudeRoot: filepath.Join(dir, fmt.Sprintf("dev%d-claude", i+1)),
			log:        &bytes.Buffer{},
		})
	}
	st.sampler = startResourceSampler(st.hubDB, 250*time.Millisecond)
	t.Cleanup(st.sampler.stopAndWait)

	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveArgs := append([]string{"child-serve",
		"--db", st.hubDB, "--state", st.hubState}, freshnessObserveArgs(opts.observe)...)
	st.serveCmd = exec.Command(os.Args[0], serveArgs...)
	st.serveCmd.Env = append(os.Environ(), "OHYA_TEST_DERP_REGION_FILE="+regionFile)
	st.serveCmd.Stderr = st.serveLog
	serveOut, err := st.serveCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.serveCmd.Start(); err != nil {
		t.Fatal(err)
	}
	st.sampler.track("serve", st.serveCmd)
	t.Cleanup(func() {
		if st.serveCmd.ProcessState == nil {
			_ = st.serveCmd.Process.Kill()
			st.serveCmd.Wait()
		}
	})
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(serveOut)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	waitServeReady(t, lines, st.serveLog)

	for i, dev := range st.extraDevices {
		corpus := fixgen.Corpus{
			Sessions:            opts.deviceMiB / 2,
			MainMeanBytes:       2 << 20,
			SubagentsPerSession: 2,
			Seed:                int64(700 + i + 1),
		}
		if _, err := fixgen.Write(dev.claudeRoot, corpus); err != nil {
			t.Fatal(err)
		}
		var invOut bytes.Buffer
		if err := runDevicesInvite([]string{"--state", st.hubState}, &invOut, os.Stderr); err != nil {
			t.Fatal(err)
		}
		dev.code = strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]
		if _, _, err := parseJoinCode(dev.code); err != nil {
			t.Fatalf("join code = %q; want an ohya1 code: %v", dev.code, err)
		}
	}

	var invOut bytes.Buffer
	if err := runDevicesInvite([]string{"--state", st.hubState}, &invOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]
	if _, _, err := parseJoinCode(code); err != nil {
		t.Fatalf("join code = %q; want an ohya1 code: %v", code, err)
	}

	joinArgs := append([]string{"child-join",
		"--state", st.devState,
		"--claude-root", filepath.Dir(st.sessionPath), "--codex-root", "", "--pi-root", "",
		"--interval", opts.uploadEvery.String()},
		freshnessObserveArgs(opts.observe)...)
	joinArgs = append(joinArgs, code)
	st.joinCmd = exec.Command(os.Args[0], joinArgs...)
	st.joinCmd.Stderr = st.joinLog
	if err := st.joinCmd.Start(); err != nil {
		t.Fatal(err)
	}
	st.sampler.track("join", st.joinCmd)
	t.Cleanup(func() {
		if st.joinCmd.ProcessState == nil {
			stopChild(t, st.joinCmd, st.joinLog, 30*time.Second)
		}
	})

	upCtx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	for {
		events, sessions, _ := hubCounts(t, st.hubDB)
		if events >= 16 && sessions >= 1 {
			break
		}
		select {
		case <-upCtx.Done():
			t.Fatalf("fixture never reached the hub; serve stderr:\n%s\njoin stderr:\n%s",
				st.serveLog.String(), st.joinLog.String())
		case <-time.After(500 * time.Millisecond):
		}
	}

	mcpCmd := exec.Command(os.Args[0], append([]string{"child-mcp",
		"--remote", "--state", st.devState}, freshnessObserveArgs(opts.observe)...)...)
	client := mcp.NewClient(&mcp.Implementation{Name: "freshness-observer", Version: "v0"}, nil)
	st.mcpCtx, st.mcpCancel = context.WithTimeout(context.Background(), 30*time.Minute)
	session, err := client.Connect(st.mcpCtx, &mcp.CommandTransport{Command: mcpCmd}, nil)
	if err != nil {
		t.Fatalf("remote mcp connect: %v; serve stderr:\n%s\njoin stderr:\n%s",
			err, st.serveLog.String(), st.joinLog.String())
	}
	st.session = session
	t.Cleanup(func() {
		session.Close()
		st.mcpCancel()
	})
	return st
}

func freshnessSQL(t *testing.T, st *freshnessStack, query string) ([][]any, time.Duration, error) {
	t.Helper()
	start := time.Now()
	res, err := st.session.CallTool(st.mcpCtx, &mcp.CallToolParams{
		Name:      "sql",
		Arguments: map[string]any{"query": query},
	})
	dur := time.Since(start)
	if err != nil {
		return nil, dur, err
	}
	if res.IsError {
		return nil, dur, fmt.Errorf("sql tool rejected %q: %+v", query, res)
	}
	var payload struct {
		Rows [][]any `json:"rows"`
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil, dur, err
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, dur, fmt.Errorf("decode sql result %s: %w", raw, err)
	}
	return payload.Rows, dur, nil
}

func freshnessMarkerRows(t *testing.T, st *freshnessStack, lastID int64, prefix string) (ids []int64, texts []string, dur time.Duration, err error) {
	t.Helper()
	query := fmt.Sprintf("SELECT CAST(id AS TEXT), text FROM events WHERE id > %d AND text LIKE '%%%s%%' ORDER BY id", lastID, prefix)
	rows, dur, err := freshnessSQL(t, st, query)
	if err != nil {
		return nil, nil, dur, err
	}
	for _, row := range rows {
		if len(row) != 2 {
			return nil, nil, dur, fmt.Errorf("marker query returned a %d-column row", len(row))
		}
		id, convErr := strconv.ParseInt(fmt.Sprint(row[0]), 10, 64)
		if convErr != nil {
			return nil, nil, dur, fmt.Errorf("marker row id %v: %w", row[0], convErr)
		}
		ids = append(ids, id)
		texts = append(texts, fmt.Sprint(row[1]))
	}
	return ids, texts, dur, nil
}

func freshnessRunID(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func freshnessPct(sorted []time.Duration, pct int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (pct*len(sorted) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	return sorted[idx-1]
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (r *freshnessReport) counts() (visible, timedOut, overdue, outstanding int) {
	for _, s := range r.samples {
		switch {
		case s.timedOut:
			timedOut++
		case s.visible:
			visible++
			if s.overdue {
				overdue++
			}
		}
	}
	return visible, timedOut, overdue, timedOut
}

func (r *freshnessReport) logTo(t *testing.T, st *freshnessStack) {
	t.Helper()
	lags := make([]time.Duration, len(r.samples))
	for i, s := range r.samples {
		lags[i] = s.lag
	}
	sort.Slice(lags, func(a, b int) bool { return lags[a] < lags[b] })
	qd := make([]time.Duration, len(r.queryDurs))
	copy(qd, r.queryDurs)
	sort.Slice(qd, func(a, b int) bool { return qd[a] < qd[b] })
	visible, timedOut, overdue, outstanding := r.counts()
	t.Logf("freshness %s: appended=%d visible=%d timedOut=%d overdue=%d outstandingAtDrain=%d deliveredLate=%d neverDelivered=%d",
		r.run, len(r.samples), visible, timedOut, overdue, outstanding, r.deliveredLate, r.neverDelivered)
	t.Logf("freshness %s: uploadInterval=%s appendEvery=%s pollEvery=%s timeout=%s phaseOffset=%s seed=%d observe=%s",
		r.run, r.opts.uploadEvery, r.opts.appendEvery, r.opts.pollEvery, r.opts.timeout, r.phaseOffset, r.opts.seed, r.opts.observe)
	t.Logf("freshness %s: lag p50=%s p95=%s p99=%s max=%s (timed-out samples stay in the distribution at drain lower bounds)",
		r.run, freshnessPct(lags, 50).Round(time.Millisecond), freshnessPct(lags, 95).Round(time.Millisecond),
		freshnessPct(lags, 99).Round(time.Millisecond), lags[len(lags)-1].Round(time.Millisecond))
	t.Logf("freshness %s: poll queries=%d errors=%d queryDur p50=%s p95=%s max=%s warmup=%s; pollEvery bounds visibility from above",
		r.run, r.queries, r.queryErrors, freshnessPct(qd, 50).Round(time.Microsecond),
		freshnessPct(qd, 95).Round(time.Microsecond), qd[len(qd)-1].Round(time.Microsecond), r.warmupQuery.Round(time.Millisecond))
	for i, txt := range r.queryErrTexts {
		t.Logf("freshness %s: poll error %d: %s", r.run, i+1, txt)
	}
	t.Logf("freshness %s: hub events=%d; %s %s/%s", r.run, r.hubEvents, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if r.backlogWritten > 0 || r.backlogAcked > 0 {
		t.Logf("freshness %s: backlog writer bps=%d written=%d acked=%d outstanding=%d",
			r.run, r.opts.backlogBPS, r.backlogWritten, r.backlogAcked, r.backlogWritten-r.backlogAcked)
	}
	for _, dp := range r.deviceProgress {
		t.Logf("freshness %s: device %s fileBytes=%d acked=%d outstanding=%d",
			r.run, dp.deviceID, dp.fileBytes, dp.ackedBytes, dp.fileBytes-dp.ackedBytes)
	}
	for _, q := range r.loadedQueries {
		durs := append([]time.Duration(nil), q.durs...)
		sort.Slice(durs, func(a, b int) bool { return durs[a] < durs[b] })
		if len(durs) == 0 {
			t.Logf("freshness %s: loaded query %s: n=0 errors=%d timeouts=%d", r.run, q.name, q.errors, q.timeouts)
			continue
		}
		t.Logf("freshness %s: loaded query %s: n=%d p50=%s p95=%s max=%s errors=%d timeouts=%d",
			r.run, q.name, len(durs), freshnessPct(durs, 50).Round(time.Millisecond),
			freshnessPct(durs, 95).Round(time.Millisecond), durs[len(durs)-1].Round(time.Millisecond),
			q.errors, q.timeouts)
	}
	if st != nil && st.sampler != nil {
		procs, dbBytes, walPeak, samples := st.sampler.peaks()
		meanRSS, maxRSS, idleCPU, idleWindow := st.sampler.idleStats()
		if idleWindow > 0 {
			t.Logf("freshness %s: hub idle over %s: meanRSS=%dMiB maxRSS=%dMiB cpu=%.2fs",
				r.run, idleWindow, meanRSS>>10, maxRSS>>10, idleCPU)
		}
		for _, p := range procs {
			t.Logf("freshness %s: %s peakRSS=%dMiB cpuSec=%.2f", r.run, p.name, p.peakRSSKB>>10, p.cpuSec)
		}
		t.Logf("freshness %s: db=%dMiB walPeak=%dMiB (sampler n=%d every 250ms)",
			r.run, dbBytes>>20, walPeak>>20, samples)
	}
}

var freshnessLoadedQueries = []struct {
	name  string
	query string
}{
	{"FTS-selective", `SELECT e.id, substr(e.text, 1, 200)
		FROM events_fts JOIN events e ON e.id = events_fts.rowid
		WHERE events_fts MATCH 'authentication' ORDER BY events_fts.rank LIMIT 10`},
	{"FTS-wide", `SELECT e.id, substr(e.text, 1, 200)
		FROM events_fts JOIN events e ON e.id = events_fts.rowid
		WHERE events_fts MATCH 'session' ORDER BY events_fts.rank LIMIT 10`},
	{"session-token-totals", `SELECT s.id, s.first_prompt,
			sum(u.input_tokens) AS input_tokens, sum(u.output_tokens) AS output_tokens
		FROM sessions s
		LEFT JOIN session_token_usage u ON u.session_id = s.id
		GROUP BY s.id`},
	{"recent-sessions", `SELECT id, agent, first_prompt, event_count
		FROM sessions ORDER BY last_event_at_ms DESC LIMIT 20`},
	{"session-event-range", `SELECT e.id, e.time_ms, substr(e.text, 1, 200)
		FROM events e WHERE e.session_id =
			(SELECT id FROM sessions ORDER BY last_event_at_ms DESC LIMIT 1)
			AND e.time_ms >= 0
		ORDER BY e.time_ms LIMIT 500`},
	{"annotation-paging", `SELECT e.id FROM events e
		WHERE e.id > 0 AND NOT EXISTS (
			SELECT 1 FROM event_annotations a
			WHERE a.event_id = e.id AND a.writer_id = 'dev-bench'
				AND a.namespace = 'security' AND a.key = 'review')
		ORDER BY e.id LIMIT 100`},
}

func deviceUploadProgress(t *testing.T, dbPath, deviceID string, paths ...string) (fileBytes, ackedBytes int64) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cursors := map[string]int64{}
	rows, err := db.Query(`SELECT path, cursor_bytes FROM sources WHERE device_id = ?`, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var c int64
		if err := rows.Scan(&p, &c); err != nil {
			t.Fatal(err)
		}
		cursors[p] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, root := range paths {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			fileBytes += info.Size()
			ackedBytes += cursors[p]
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return fileBytes, ackedBytes
}

type loadedQueryRunner struct {
	stats map[string]*loadedQueryStats
	done  chan struct{}
}

func startLoadedQuerySession(t *testing.T, st *freshnessStack, stop <-chan struct{}, timeout, period time.Duration) *loadedQueryRunner {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"child-mcp",
		"--remote", "--state", st.devState}, freshnessObserveArgs(st.observe)...)...)
	client := mcp.NewClient(&mcp.Implementation{Name: "freshness-load", Version: "v0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("loaded-query connect: %v; serve stderr:\n%s", err, st.serveLog.String())
	}
	t.Cleanup(func() {
		session.Close()
		cancel()
	})
	r := &loadedQueryRunner{
		stats: map[string]*loadedQueryStats{},
		done:  make(chan struct{}),
	}
	for _, q := range freshnessLoadedQueries {
		r.stats[q.name] = &loadedQueryStats{name: q.name}
	}
	call := func(q struct{ name, query string }) {
		stat := r.stats[q.name]
		start := time.Now()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "sql", Arguments: map[string]any{"query": q.query},
		})
		dur := time.Since(start)
		if err != nil {
			stat.errors++
			if strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "timeout") {
				stat.timeouts++
			}
			return
		}
		if res.IsError {
			stat.errors++
			if dur >= timeout {
				stat.timeouts++
			}
			return
		}
		if dur >= timeout {
			stat.timeouts++
		}
		stat.durs = append(stat.durs, dur)
	}
	for _, q := range freshnessLoadedQueries {
		call(q)
	}
	go func() {
		defer close(r.done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(period):
			}
			for _, q := range freshnessLoadedQueries {
				select {
				case <-stop:
					return
				default:
				}
				call(q)
			}
		}
	}()
	return r
}

func (r *loadedQueryRunner) collect() []loadedQueryStats {
	<-r.done
	out := make([]loadedQueryStats, 0, len(freshnessLoadedQueries))
	for _, q := range freshnessLoadedQueries {
		out = append(out, *r.stats[q.name])
	}
	return out
}

func startBacklogWriter(t *testing.T, st *freshnessStack, run string, bps int, stop <-chan struct{}, written *int64) {
	t.Helper()
	go func() {
		f, err := os.OpenFile(st.backlogPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		tick := 100 * time.Millisecond
		chunk := int64(bps) * int64(tick) / int64(time.Second)
		if chunk < 512 {
			chunk = 512
		}
		var buf []byte
		n := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			buf = buf[:0]
			var batch int64
			for batch < chunk {
				n++
				id := fmt.Sprintf("ohyabacklog-%s-%08d", run, n)
				line := fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":%q,"message":{"role":"user","content":%q},"timestamp":"2026-11-13T11:00:00Z"}`+"\n",
					id, backlogSessionID, id+" the backlog writer keeps a second source busy with ordinary session text")
				buf = append(buf, line...)
				batch += int64(len(line))
			}
			if _, err := f.Write(buf); err != nil {
				return
			}
			atomic.AddInt64(written, int64(len(buf)))
			select {
			case <-stop:
				return
			case <-time.After(tick):
			}
		}
	}()
}

func measureFreshness(t *testing.T, st *freshnessStack, opts freshnessOptions) *freshnessReport {
	t.Helper()
	run := freshnessRunID(t)
	rng := rand.New(rand.NewPCG(opts.seed, opts.seed^0x9e3779b97f4a7c15))
	prefix := "ohyafresh-" + run
	rep := &freshnessReport{run: run, opts: opts, samples: make([]freshnessSample, opts.samples)}
	for i := range rep.samples {
		rep.samples[i].marker = fmt.Sprintf("%s-%06d", prefix, i)
	}
	byMarker := make(map[string]int, len(rep.samples))
	for i, s := range rep.samples {
		byMarker[s.marker] = i
	}

	warmStart := time.Now()
	if _, _, err := freshnessSQL(t, st, "SELECT count(*) FROM events"); err != nil {
		t.Fatalf("warmup query: %v; serve stderr:\n%s\njoin stderr:\n%s",
			err, st.serveLog.String(), st.joinLog.String())
	}
	rep.warmupQuery = time.Since(warmStart)
	rep.phaseOffset = time.Duration(rng.Int64N(int64(opts.uploadEvery)))

	if st.sampler != nil {
		st.sampler.beginIdle()
		time.Sleep(opts.idleSampleFor)
		st.sampler.endIdle(opts.idleSampleFor)
	}

	loadStop := make(chan struct{})
	for i, dev := range st.extraDevices {
		devArgs := append([]string{"child-join",
			"--state", dev.stateDir,
			"--claude-root", dev.claudeRoot, "--codex-root", "", "--pi-root", "",
			"--interval", opts.uploadEvery.String()},
			freshnessObserveArgs(opts.observe)...)
		dev.cmd = exec.Command(os.Args[0], append(devArgs, dev.code)...)
		dev.cmd.Stderr = dev.log
		if err := dev.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		st.sampler.track(fmt.Sprintf("join%d", i+2), dev.cmd)
		d := dev
		t.Cleanup(func() {
			if d.cmd.ProcessState == nil {
				stopChild(t, d.cmd, d.log, 30*time.Second)
			}
		})
	}
	if len(st.extraDevices) > 0 {
		time.Sleep(opts.pairLead)
	}
	loadStart := time.Now()
	var queryRunner *loadedQueryRunner
	if opts.queriesEvery > 0 {
		queryRunner = startLoadedQuerySession(t, st, loadStop, opts.queryTimeout, opts.queriesEvery)
	}
	if opts.backlogBPS > 0 {
		startBacklogWriter(t, st, run, opts.backlogBPS, loadStop, &rep.backlogWritten)
	}

	var mu sync.Mutex
	appendsDone := make(chan time.Time, 1)
	appendErr := make(chan error, 1)
	go func() {
		time.Sleep(rep.phaseOffset)
		f, err := os.OpenFile(st.sessionPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			appendErr <- err
			return
		}
		var writeErr error
		for i := range rep.samples {
			marker := rep.samples[i].marker
			line := fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":%q,"message":{"role":"user","content":%q},"timestamp":"2026-11-13T10:00:00Z"}`+"\n",
				marker, freshnessSessionID, marker)
			if _, writeErr = f.WriteString(line); writeErr != nil {
				break
			}
			mu.Lock()
			rep.samples[i].appendedAt = time.Now()
			mu.Unlock()
			if i == len(rep.samples)-1 {
				break
			}
			time.Sleep(time.Duration(float64(opts.appendEvery) * (0.9 + 0.2*rng.Float64())))
		}
		closeErr := f.Close()
		if writeErr != nil {
			appendErr <- writeErr
			return
		}
		if closeErr != nil {
			appendErr <- closeErr
			return
		}
		mu.Lock()
		last := rep.samples[len(rep.samples)-1].appendedAt
		mu.Unlock()
		appendsDone <- last
	}()

	var visibleCount int
	var lastID int64
	drainDeadline := time.Time{}
	nextPoll := time.Now().Add(opts.pollEvery)
	started := time.Now()
	budget := time.Duration(opts.samples)*opts.appendEvery + opts.timeout + 4*time.Minute
	for {
		if st.joinCmd.ProcessState != nil || st.serveCmd.ProcessState != nil {
			t.Fatalf("stack child exited during measurement; serve stderr:\n%s\njoin stderr:\n%s",
				st.serveLog.String(), st.joinLog.String())
		}
		select {
		case err := <-appendErr:
			t.Fatalf("append workload failed: %v", err)
		case last := <-appendsDone:
			drainDeadline = last.Add(opts.timeout)
		default:
		}
		if visibleCount == len(rep.samples) {
			break
		}
		if !drainDeadline.IsZero() && time.Now().After(drainDeadline) {
			break
		}
		if time.Since(started) > budget {
			t.Fatalf("observer budget %s exceeded; serve stderr:\n%s\njoin stderr:\n%s",
				budget, st.serveLog.String(), st.joinLog.String())
		}
		if sleep := time.Until(nextPoll); sleep > 0 {
			time.Sleep(sleep)
		}
		nextPoll = time.Now().Add(opts.pollEvery)

		ids, texts, dur, err := freshnessMarkerRows(t, st, lastID, prefix)
		rep.queries++
		rep.queryDurs = append(rep.queryDurs, dur)
		if err != nil {
			rep.queryErrors++
			if len(rep.queryErrTexts) < 5 {
				rep.queryErrTexts = append(rep.queryErrTexts, fmt.Sprintf("%v (after %s)", err, dur.Round(time.Millisecond)))
			}
			continue
		}
		seenAt := time.Now()
		for i, text := range texts {
			if ids[i] > lastID {
				lastID = ids[i]
			}
			idx, ok := byMarker[text]
			if !ok {
				continue
			}
			mu.Lock()
			s := &rep.samples[idx]
			if !s.visible && !s.appendedAt.IsZero() {
				s.visible = true
				s.lag = seenAt.Sub(s.appendedAt)
				s.queryDur = dur
				s.overdue = s.lag > opts.timeout
				visibleCount++
			}
			mu.Unlock()
		}
	}

	drainEnd := time.Now()
	close(loadStop)
	rep.loadWindow = drainEnd.Sub(loadStart)
	if queryRunner != nil {
		rep.loadedQueries = queryRunner.collect()
	}
	var timedCount int
	for i := range rep.samples {
		if !rep.samples[i].visible {
			rep.samples[i].timedOut = true
			rep.samples[i].lag = drainEnd.Sub(rep.samples[i].appendedAt)
			timedCount++
		}
	}
	if timedCount > 0 {
		healthDeadline := time.Now().Add(time.Minute)
		for time.Now().Before(healthDeadline) && rep.deliveredLate < timedCount {
			ids, texts, _, err := freshnessMarkerRows(t, st, lastID, prefix)
			if err == nil {
				for i, text := range texts {
					if ids[i] > lastID {
						lastID = ids[i]
					}
					if idx, ok := byMarker[text]; ok && !rep.samples[idx].visible && rep.samples[idx].timedOut {
						rep.samples[idx].lateDelivered = true
						rep.deliveredLate++
					}
				}
			}
			time.Sleep(2 * opts.pollEvery)
		}
		rep.neverDelivered = timedCount - rep.deliveredLate
	}

	if rows, _, err := freshnessSQL(t, st, "SELECT CAST(count(*) AS TEXT) FROM events"); err == nil && len(rows) == 1 {
		if n, convErr := strconv.Atoi(fmt.Sprint(rows[0][0])); convErr == nil {
			rep.hubEvents = n
		}
	}
	if opts.backlogBPS > 0 {
		devID := readDeviceID(t, st.devState)
		rep.backlogAcked = ackedOfPath(t, st.hubDB, devID, st.backlogPath)
	}
	for _, dev := range st.extraDevices {
		dev.deviceID = readDeviceID(t, dev.stateDir)
		fileBytes, acked := deviceUploadProgress(t, st.hubDB, dev.deviceID, dev.claudeRoot)
		rep.deviceProgress = append(rep.deviceProgress,
			deviceProgressLine{deviceID: dev.deviceID, fileBytes: fileBytes, ackedBytes: acked})
	}
	return rep
}

func ackedOfPath(t *testing.T, dbPath, deviceID, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var acked int64
	if err := db.QueryRow(`SELECT cursor_bytes FROM sources WHERE device_id = ? AND path = ?`,
		deviceID, path).Scan(&acked); err != nil {
		return 0
	}
	return acked
}

func TestFreshnessSmoke(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046); the stack runs on the development platform")
	}
	if testing.Short() {
		t.Skip("end-to-end stack test")
	}
	opts := freshnessOptionsFromEnv(t)
	st := startFreshnessStack(t, opts)
	rep := measureFreshness(t, st, opts)
	rep.logTo(t, st)
	if opts.observe == "on" {
		assertRemoteUsageRecords(t, st, rep)
	}
	visible, timedOut, overdue, outstanding := rep.counts()
	loaded := opts.backlogBPS > 0 || opts.devices > 1 || opts.queriesEvery > 0
	if rep.queryErrors > 0 {
		t.Errorf("freshness smoke: %d of %d poll queries failed", rep.queryErrors, rep.queries)
	}
	if !loaded && (timedOut != 0 || overdue != 0) {
		t.Errorf("freshness smoke: %d samples timed out and %d exceeded the %s budget on the default configuration",
			timedOut, overdue, opts.timeout)
	}
	if rep.neverDelivered != 0 {
		t.Errorf("freshness smoke: %d of %d outstanding records never reached the hub",
			rep.neverDelivered, outstanding)
	}
	if rep.queryErrors > 0 || rep.neverDelivered != 0 {
		t.Logf("freshness smoke: serve stderr tail:\n%s", tailLines(st.serveLog.String(), 15))
		t.Logf("freshness smoke: join stderr tail:\n%s", tailLines(st.joinLog.String(), 15))
	}
	if visible == 0 {
		t.Fatalf("freshness smoke: no sample ever became visible; serve stderr:\n%s\njoin stderr:\n%s",
			st.serveLog.String(), st.joinLog.String())
	}
}

func assertRemoteUsageRecords(t *testing.T, st *freshnessStack, rep *freshnessReport) {
	t.Helper()
	waitUsageRecords(t, st.devState, 1)
	records := readUsageRecords(t, st.devState)
	deviceID, err := device.LoadOrCreate(st.devState)
	if err != nil {
		t.Fatal(err)
	}
	ok := 0
	for _, record := range records {
		if err := record.Validate(); err != nil {
			t.Fatalf("freshness smoke: device-side usage record invalid: %v", err)
		}
		if record.Path != observe.UsagePathRemote || record.DeviceID != deviceID {
			t.Fatalf("freshness smoke: device-side record path/device = %s/%s, want remote/%s",
				record.Path, record.DeviceID, deviceID)
		}
		if record.Tool != observe.UsageToolSQL {
			t.Fatalf("freshness smoke: device-side record tool = %s, want sql", record.Tool)
		}
		if record.Client != observe.UsageClientOther {
			t.Fatalf("freshness smoke: device-side record client = %s, want other", record.Client)
		}
		if record.PhaseMS.TotalMS <= 0 {
			t.Fatalf("freshness smoke: device-side record total_ms = %v, want the client-observed time",
				record.PhaseMS.TotalMS)
		}
		if record.Outcome == observe.UsageOutcomeOK {
			ok++
		}
	}
	if ok == 0 {
		t.Fatalf("freshness smoke: %d device-side records hold no ok outcome", len(records))
	}
	if len(records) < rep.queries-rep.queryErrors {
		t.Fatalf("freshness smoke: %d device-side records for %d successful poll queries",
			len(records), rep.queries-rep.queryErrors)
	}
}
