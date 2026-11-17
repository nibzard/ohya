package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nibzard/ohya/internal/fixgen"
)

func TestWorkloadImport(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046)")
	}
	if testing.Short() {
		t.Skip("end-to-end stack test")
	}
	if os.Getenv("OHYA_WORKLOAD_IMPORT") != "1" {
		t.Skip("set OHYA_WORKLOAD_IMPORT=1 to run the initial-import workload (generates the import fixture)")
	}

	dir := t.TempDir()
	importRoot := filepath.Join(dir, "corpus")
	mix := fixgen.ImportMix(fixgen.ImportScale, 7)
	corpora := fixgen.GenerateMix(mix)
	totalBytes, err := fixgen.WriteAgentFiles(importRoot, corpora)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := fixgen.MeasureMix(mix, corpora)
	if err != nil {
		t.Fatal(err)
	}
	var wantEvents int64
	var files int
	for _, s := range stats {
		wantEvents += s.Events
		files += s.Files
		t.Logf("fixture %s: sessions=%d files=%d events=%d bytes=%d",
			s.Agent, s.Sessions, s.Files, s.Events, s.Bytes)
	}
	t.Logf("fixture total: files=%d events=%d bytes=%.1f MiB",
		files, wantEvents, float64(totalBytes)/(1<<20))

	hubDB := filepath.Join(dir, "hub", "hub.db")
	hubState := filepath.Join(dir, "hub-state")
	devState := filepath.Join(dir, "dev-state")
	regionFile := writeDERPRegionFile(t, startLocalDERP(t))

	startServeChild := func(sampler *resourceSampler) (*exec.Cmd, *bytes.Buffer) {
		t.Helper()
		log := &bytes.Buffer{}
		cmd := exec.Command(os.Args[0], "child-serve",
			"--db", hubDB, "--state", hubState)
		cmd.Env = append(os.Environ(), "OHYA_TEST_DERP_REGION_FILE="+regionFile)
		cmd.Stderr = log
		serveOut, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if sampler != nil {
			sampler.track("serve", cmd)
		}
		lines := make(chan string, 16)
		go func() {
			sc := bufio.NewScanner(serveOut)
			for sc.Scan() {
				lines <- sc.Text()
			}
		}()
		waitServeReady(t, lines, log)
		return cmd, log
	}

	sampler := startResourceSampler(hubDB, 200*time.Millisecond)
	serveCmd, serveLog := startServeChild(sampler)
	t.Cleanup(func() {
		if serveCmd.ProcessState == nil {
			_ = serveCmd.Process.Kill()
			serveCmd.Wait()
		}
	})

	var invOut bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState}, &invOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]
	if _, _, err := parseJoinCode(code); err != nil {
		t.Fatalf("join code = %q: %v", code, err)
	}

	startJoinChild := func(code string) (*exec.Cmd, *bytes.Buffer) {
		t.Helper()
		log := &bytes.Buffer{}
		args := []string{
			"--state", devState,
			"--claude-root", filepath.Join(importRoot, "claude-code"),
			"--codex-root", filepath.Join(importRoot, "codex"),
			"--pi-root", filepath.Join(importRoot, "pi"),
			"--interval", "2s",
		}
		if code != "" {
			args = append(args, code)
		}
		cmd := exec.Command(os.Args[0], append([]string{"child-join"}, args...)...)
		cmd.Stderr = log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, log
	}

	importStart := time.Now()
	joinCmd, joinLog := startJoinChild(code)
	sampler.track("join", joinCmd)
	t.Cleanup(func() {
		if joinCmd.ProcessState == nil {
			_ = joinCmd.Process.Kill()
			joinCmd.Wait()
		}
	})

	deadline := importStart.Add(10 * time.Minute)
	for {
		events, _, _ := hubCounts(t, hubDB)
		if events >= int(wantEvents) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("initial import still at %d of %d events after %s; serve stderr:\n%s\njoin stderr:\n%s",
				events, wantEvents, time.Since(importStart), serveLog.String(), joinLog.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
	timeToMemory := time.Since(importStart)
	sampler.stopAndWait()
	procs, dbBytes, walPeak, samples := sampler.peaks()
	importCPUSec := 0.0
	for _, p := range procs {
		importCPUSec += p.cpuSec
		t.Logf("import %s: peakRSS=%dMiB cpuSec=%.2f", p.name, p.peakRSSKB>>10, p.cpuSec)
	}
	t.Logf("import: timeToMemory=%s (join start to last event, polled at 500ms) bytes=%d rate=%.2f MiB/s cpuSec=%.2f",
		timeToMemory.Round(time.Millisecond), totalBytes,
		float64(totalBytes)/(1<<20)/timeToMemory.Seconds(), importCPUSec)
	t.Logf("import: db=%dMiB walPeak=%dMiB samplerSamples=%d every 200ms",
		dbBytes>>20, walPeak>>20, samples)

	appendMarker := func(marker string) string {
		t.Helper()
		var sessionFile string
		err := filepath.WalkDir(filepath.Join(importRoot, "claude-code"), func(p string, d fs.DirEntry, err error) error {
			if err != nil || sessionFile != "" {
				return err
			}
			if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
				sessionFile = p
			}
			return nil
		})
		if err != nil || sessionFile == "" {
			t.Fatalf("no claude-code fixture file to append to (err %v)", err)
		}
		f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":%q,"message":{"role":"user","content":%q},"timestamp":"2026-11-13T12:00:00Z"}`+"\n",
			marker, freshnessSessionID, marker)
		_, werr := f.WriteString(line)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			t.Fatalf("append marker: write %v, close %v", werr, cerr)
		}
		return sessionFile
	}

	markerVisible := func(marker string) bool {
		db, err := sql.Open("sqlite3", "file:"+hubDB+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE text LIKE ?`,
			"%"+marker+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	waitMarker := func(marker string, timeout time.Duration) bool {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			if markerVisible(marker) {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	stopChild(t, joinCmd, joinLog, 30*time.Second)
	appendMarker("ohyarestart-device")
	deviceRestartStart := time.Now()
	joinCmd, joinLog = startJoinChild("")
	t.Cleanup(func() {
		if joinCmd.ProcessState == nil {
			_ = joinCmd.Process.Kill()
			joinCmd.Wait()
		}
	})
	if waitMarker("ohyarestart-device", 2*time.Minute) {
		t.Logf("device restart: recoveryToVisibility=%s (join restart, full source re-probe, marker upload; polled at 100ms)",
			time.Since(deviceRestartStart).Round(time.Millisecond))
	} else {
		t.Errorf("device restart: marker never reached the hub within 2m; join stderr:\n%s", joinLog.String())
	}

	stopChild(t, serveCmd, serveLog, 30*time.Second)
	appendMarker("ohyarestart-hub")
	hubRestartStart := time.Now()
	serveCmd, serveLog = startServeChild(nil)
	t.Cleanup(func() {
		if serveCmd.ProcessState == nil {
			_ = serveCmd.Process.Kill()
			serveCmd.Wait()
		}
	})
	if waitMarker("ohyarestart-hub", 3*time.Minute) {
		t.Logf("hub restart: recoveryToVisibility=%s (serve restart, join reconnect with backoff, marker upload; polled at 100ms)",
			time.Since(hubRestartStart).Round(time.Millisecond))
	} else {
		t.Errorf("hub restart: the joined device never re-uploaded within 3m (recovery regression, docs/performance.md workload matrix; a join-only restart reconnects); serve stderr:\n%s\njoin stderr:\n%s",
			serveLog.String(), joinLog.String())
	}

	events, sessions, _ := hubCounts(t, hubDB)
	want := int(wantEvents)
	for _, marker := range []string{"ohyarestart-device", "ohyarestart-hub"} {
		if markerVisible(marker) {
			want++
		}
	}
	if events != want {
		t.Fatalf("hub holds %d events after replay and restarts; want exactly %d (no duplicates, no losses)", events, want)
	}
	t.Logf("final: events=%d sessions=%d (idempotent across device and hub restarts)", events, sessions)
}
