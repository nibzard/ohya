package main

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const freshnessSessionID = "00000000-0000-4000-8000-0000000000a1"

type freshnessOptions struct {
	samples     int
	appendEvery time.Duration
	pollEvery   time.Duration
	timeout     time.Duration
	uploadEvery time.Duration
	seed        uint64
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
}

type freshnessStack struct {
	hubDB       string
	hubState    string
	devState    string
	sessionPath string
	serveCmd    *exec.Cmd
	serveLog    *bytes.Buffer
	joinCmd     *exec.Cmd
	joinLog     *bytes.Buffer
	session     *mcp.ClientSession
	mcpCtx      context.Context
	mcpCancel   context.CancelFunc
}

func freshnessIntEnv(t *testing.T, name string, def int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("%s = %q: want a positive integer", name, raw)
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
	}
}

func startFreshnessStack(t *testing.T, uploadEvery time.Duration) *freshnessStack {
	t.Helper()
	dir := t.TempDir()
	st := &freshnessStack{
		hubState:    filepath.Join(dir, "hub-state"),
		hubDB:       filepath.Join(dir, "hub", "hub.db"),
		devState:    filepath.Join(dir, "dev-state"),
		sessionPath: filepath.Join(dir, "claude-projects", "session.jsonl"),
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
	ctrlAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	stunAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))

	st.serveCmd = exec.Command(os.Args[0], "child-serve",
		"--db", st.hubDB, "--state", st.hubState,
		"--control-listen", ctrlAddr, "--stun-listen", stunAddr)
	st.serveCmd.Stderr = st.serveLog
	serveOut, err := st.serveCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.serveCmd.Start(); err != nil {
		t.Fatal(err)
	}
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

	var invOut bytes.Buffer
	if err := runDevicesInvite([]string{"--state", st.hubState}, &invOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]
	if !strings.Contains(code, ":") {
		t.Fatalf("join code = %q; want invitation and preauth key halves", code)
	}

	st.joinCmd = exec.Command(os.Args[0], "child-join",
		"--control", "http://"+ctrlAddr,
		"--state", st.devState,
		"--claude-root", filepath.Dir(st.sessionPath), "--codex-root", "", "--pi-root", "",
		"--interval", uploadEvery.String(),
		code)
	st.joinCmd.Stderr = st.joinLog
	if err := st.joinCmd.Start(); err != nil {
		t.Fatal(err)
	}
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

	mcpCmd := exec.Command(os.Args[0], "child-mcp",
		"--remote", "--control", "http://"+ctrlAddr, "--state", st.devState)
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
	query := fmt.Sprintf("SELECT id, text FROM events WHERE id > %d AND text LIKE '%%%s%%' ORDER BY id", lastID, prefix)
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

func (r *freshnessReport) logTo(t *testing.T) {
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
	t.Logf("freshness %s: uploadInterval=%s appendEvery=%s pollEvery=%s timeout=%s phaseOffset=%s seed=%d",
		r.run, r.opts.uploadEvery, r.opts.appendEvery, r.opts.pollEvery, r.opts.timeout, r.phaseOffset, r.opts.seed)
	t.Logf("freshness %s: lag p50=%s p95=%s p99=%s max=%s (timed-out samples stay in the distribution at drain lower bounds)",
		r.run, freshnessPct(lags, 50).Round(time.Millisecond), freshnessPct(lags, 95).Round(time.Millisecond),
		freshnessPct(lags, 99).Round(time.Millisecond), lags[len(lags)-1].Round(time.Millisecond))
	t.Logf("freshness %s: poll queries=%d errors=%d queryDur p50=%s p95=%s max=%s warmup=%s; pollEvery bounds visibility from above",
		r.run, r.queries, r.queryErrors, freshnessPct(qd, 50).Round(time.Microsecond),
		freshnessPct(qd, 95).Round(time.Microsecond), qd[len(qd)-1].Round(time.Microsecond), r.warmupQuery.Round(time.Millisecond))
	t.Logf("freshness %s: hub events=%d; %s %s/%s", r.run, r.hubEvents, runtime.Version(), runtime.GOOS, runtime.GOARCH)
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
	budget := time.Duration(opts.samples)*opts.appendEvery + opts.timeout + 2*time.Minute
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

	if rows, _, err := freshnessSQL(t, st, "SELECT count(*) FROM events"); err == nil && len(rows) == 1 {
		if n, convErr := strconv.Atoi(fmt.Sprint(rows[0][0])); convErr == nil {
			rep.hubEvents = n
		}
	}
	return rep
}

func TestFreshnessSmoke(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046); the stack runs on the development platform")
	}
	if testing.Short() {
		t.Skip("end-to-end stack test")
	}
	opts := freshnessOptionsFromEnv(t)
	st := startFreshnessStack(t, opts.uploadEvery)
	rep := measureFreshness(t, st, opts)
	rep.logTo(t)
	visible, timedOut, overdue, outstanding := rep.counts()
	if rep.queryErrors > 0 {
		t.Errorf("freshness smoke: %d of %d poll queries failed", rep.queryErrors, rep.queries)
	}
	if timedOut != 0 || overdue != 0 {
		t.Errorf("freshness smoke: %d samples timed out and %d exceeded the %s budget on the default configuration",
			timedOut, overdue, opts.timeout)
	}
	if rep.neverDelivered != 0 {
		t.Errorf("freshness smoke: %d of %d outstanding records never reached the hub",
			rep.neverDelivered, outstanding)
	}
	if visible == 0 {
		t.Fatalf("freshness smoke: no sample ever became visible; serve stderr:\n%s\njoin stderr:\n%s",
			st.serveLog.String(), st.joinLog.String())
	}
}
