package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/fixgen"
)

type uploadBenchSide struct {
	name        string
	compression string
	wireBytes   uint64
	payload     uint64
}

func TestUploadBackfillBench(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046)")
	}
	if testing.Short() {
		t.Skip("end-to-end stack benchmark")
	}
	if os.Getenv("OHYA_BENCH_UPLOAD") != "1" {
		t.Skip("set OHYA_BENCH_UPLOAD=1 to run the compressed-versus-plain backfill drain (generates a mix fixture)")
	}
	scale := fixgen.ImportScale
	if raw := os.Getenv("OHYA_BENCH_UPLOAD_SCALE"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed <= 0 || parsed > 1 {
			t.Fatalf("OHYA_BENCH_UPLOAD_SCALE = %q: want a float in (0, 1]", raw)
		}
		scale = parsed
	}

	dir := t.TempDir()
	importRoot := filepath.Join(dir, "corpus")
	mix := fixgen.ImportMix(scale, 7)
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
	for _, s := range stats {
		wantEvents += s.Events
		t.Logf("fixture %s: sessions=%d files=%d events=%d bytes=%d",
			s.Agent, s.Sessions, s.Files, s.Events, s.Bytes)
	}
	t.Logf("fixture total: events=%d bytes=%.1f MiB scale=%.4f",
		wantEvents, float64(totalBytes)/(1<<20), scale)

	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	sides := []uploadBenchSide{{name: "compressed"}, {name: "plain", compression: "off"}}
	for i := range sides {
		sides[i] = runUploadBenchSide(t, &sides[i], dir, i, importRoot, regionFile, wantEvents, totalBytes)
	}

	byName := map[string]uploadBenchSide{}
	for _, s := range sides {
		byName[s.name] = s
	}
	if c := byName["compressed"]; c.wireBytes == 0 || c.wireBytes >= c.payload {
		t.Fatalf("compressed run: wire bytes = %d, payload = %d; want wire below payload", c.wireBytes, c.payload)
	}
	if p := byName["plain"]; p.wireBytes < p.payload {
		t.Fatalf("plain run: wire bytes = %d, payload = %d; want wire at or above payload", p.wireBytes, p.payload)
	}
	ratio := float64(byName["compressed"].payload) / float64(byName["compressed"].wireBytes)
	plainOverhead := float64(byName["plain"].wireBytes) / float64(byName["plain"].payload)
	t.Logf("drain wire accounting: compressed payload/wire = %.2f, plain wire/payload = %.3f",
		ratio, plainOverhead)
}

func runUploadBenchSide(t *testing.T, side *uploadBenchSide, dir string, index int, importRoot, regionFile string, wantEvents, totalBytes int64) uploadBenchSide {
	t.Helper()
	sideDir := filepath.Join(dir, side.name)
	hubDB := filepath.Join(sideDir, "hub", "hub.db")
	hubState := filepath.Join(sideDir, "hub-state")
	devState := filepath.Join(sideDir, "dev-state")

	serveCmd, serveLog, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	sampler := startResourceSampler(hubDB, 200*time.Millisecond)
	sampler.track("serve", serveCmd)

	var invOut bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState}, &invOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]
	if _, _, err := parseJoinCode(code); err != nil {
		t.Fatalf("join code = %q: %v", code, err)
	}

	joinArgs := []string{
		"--state", devState,
		"--claude-root", filepath.Join(importRoot, "claude-code"),
		"--codex-root", filepath.Join(importRoot, "codex"),
		"--pi-root", filepath.Join(importRoot, "pi"),
		"--interval", "500ms",
		code,
	}
	joinLog := &bytes.Buffer{}
	joinCmd := exec.Command(os.Args[0], append([]string{"child-join"}, joinArgs...)...)
	joinCmd.Stderr = joinLog
	joinCmd.Env = os.Environ()
	if side.compression != "" {
		joinCmd.Env = append(joinCmd.Env, "OHYA_UPLOAD_COMPRESSION="+side.compression)
	}
	if err := joinCmd.Start(); err != nil {
		t.Fatal(err)
	}
	sampler.track("join", joinCmd)
	t.Cleanup(func() {
		if joinCmd.ProcessState == nil {
			_ = joinCmd.Process.Kill()
			joinCmd.Wait()
		}
	})

	drainStart := time.Now()
	deadline := drainStart.Add(30 * time.Minute)
	for {
		events, _, _ := hubCounts(t, hubDB)
		if events >= int(wantEvents) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s drain still at %d of %d events after %s; serve stderr:\n%s\njoin stderr:\n%s",
				side.name, events, wantEvents, time.Since(drainStart), serveLog.String(), joinLog.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
	drainWall := time.Since(drainStart)
	time.Sleep(2 * time.Second)
	sampler.stopAndWait()
	procs, dbBytes, walPeak, samples := sampler.peaks()
	stopChild(t, joinCmd, joinLog, 30*time.Second)
	side.payload, side.wireBytes = readUploadCounters(t, devState)
	for _, line := range backlogCompleteLines(joinLog.String()) {
		t.Logf("%s %s", side.name, line)
	}
	var joinCPU, serveCPU float64
	for _, p := range procs {
		if p.name == "join" {
			joinCPU = p.cpuSec
		}
		if p.name == "serve" {
			serveCPU = p.cpuSec
		}
	}
	t.Logf("%s drain: wall=%s bytes=%d rate=%.2f MiB/s deviceCPU=%.2fs hubCPU=%.2fs cpuSecPerGiB(device/hub)=%.2f/%.2f",
		side.name, drainWall.Round(time.Millisecond), totalBytes,
		float64(totalBytes)/(1<<20)/drainWall.Seconds(), joinCPU, serveCPU,
		joinCPU/(float64(totalBytes)/(1<<30)), serveCPU/(float64(totalBytes)/(1<<30)))
	t.Logf("%s hub: db=%dMiB walPeak=%dMiB samples=%d payload=%d wire=%d",
		side.name, dbBytes>>20, walPeak>>20, samples, side.payload, side.wireBytes)
	stopChild(t, serveCmd, serveLog, 30*time.Second)
	return *side
}

func readUploadCounters(t *testing.T, devState string) (payload, wire uint64) {
	t.Helper()
	snaps, err := filepath.Glob(filepath.Join(devState, "observe", "*", "snapshot.json"))
	if err != nil || len(snaps) == 0 {
		t.Fatalf("no observe snapshot under %s (err %v); the join child must stop cleanly after its final publish", devState, err)
	}
	var newest string
	var mod int64
	for _, p := range snaps {
		if st, err := os.Stat(p); err == nil && st.ModTime().UnixNano() >= mod {
			mod = st.ModTime().UnixNano()
			newest = p
		}
	}
	data, err := os.ReadFile(newest)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Counters map[string]uint64 `json:"counters"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("decode %s: %v", newest, err)
	}
	payload = snap.Counters["upload_payload_bytes"]
	wire = snap.Counters["upload_wire_bytes"]
	if payload == 0 {
		t.Fatalf("snapshot %s holds upload_payload_bytes = 0; join never uploaded through the recorder", newest)
	}
	return payload, wire
}

func backlogCompleteLines(log string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "upload backlog for") && strings.Contains(line, "complete") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}
