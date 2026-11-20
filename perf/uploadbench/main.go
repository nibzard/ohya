package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/observe/observertest"
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/uploader"
	"github.com/nibzard/ohya/internal/watch"
	"github.com/nibzard/ohya/perf/hubdata"
)

var agentDirs = []string{"claude-code", "codex", "pi"}

func main() {
	mode := flag.String("mode", "relay", "relay: shaped-link drain through the real endpoint; dict: compression levels and dictionary evaluation")
	root := flag.String("root", "", "corpus root written by genfixture -agent mix (required for both modes)")
	dbPath := flag.String("db", "", "hub database (read-only): measure ratios on real records instead of the fixture")
	mbits := flag.Float64("mbits", 3.1, "shared uplink rate in Mbit/s for the relay shape")
	rttMS := flag.Float64("rtt-ms", 130, "round-trip time the relay shape adds to each write")
	dictKB := flag.Int("dict-kb", 110, "trained dictionary size in KiB")
	maxTotal := flag.Int64("max-total-bytes", 512<<20, "read at most this many raw bytes per source for -db measurements")
	state := flag.String("state", "", "scratch state root (default: fresh temp directory)")
	out := flag.String("out", "perf/t146/uploadbench.txt", "output file (aggregate numbers only)")
	flag.Parse()

	started := time.Now()
	if err := run(*mode, *root, *dbPath, *mbits, *rttMS, *dictKB, *maxTotal, *state, *out, started); err != nil {
		fmt.Fprintln(os.Stderr, "uploadbench:", err)
		os.Exit(1)
	}
}

func run(mode, root, dbPath string, mbits, rttMS float64, dictKB int, maxTotal int64, state, out string, started time.Time) error {
	switch mode {
	case "relay":
		if root == "" {
			return fmt.Errorf("-mode relay needs -root (genfixture -agent mix -fixture import)")
		}
		return runRelay(root, mbits, rttMS, state, out, started)
	case "dict":
		return runDict(root, dbPath, dictKB, maxTotal, out, started)
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
}

type corpusFile struct {
	agent string
	path  string
	size  int64
}

func walkCorpus(root string) ([]corpusFile, int64, error) {
	var files []corpusFile
	var total int64
	for _, agent := range agentDirs {
		agentRoot := filepath.Join(root, agent)
		err := filepath.WalkDir(agentRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			files = append(files, corpusFile{agent: agent, path: p, size: info.Size()})
			total += info.Size()
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	if len(files) == 0 {
		return nil, 0, fmt.Errorf("no *.jsonl files under %s (run cmd/genfixture -agent mix first)", root)
	}
	return files, total, nil
}

type relayRun struct {
	name     string
	wall     time.Duration
	payload  uint64
	wire     uint64
	cpuSec   float64
	events   int
	logLines []string
}

func runRelay(root string, mbits, rttMS float64, stateDir, out string, started time.Time) error {
	files, total, err := walkCorpus(root)
	if err != nil {
		return err
	}
	byAgent := map[string][]corpusFile{}
	for _, f := range files {
		byAgent[f.agent] = append(byAgent[f.agent], f)
	}
	fmt.Fprintf(os.Stderr, "corpus: %d files, %.1f MiB across %d agents (rate %.1f Mbit/s, rtt %.0f ms)\n",
		len(files), float64(total)/(1<<20), len(byAgent), mbits, rttMS)

	scratch, err := scratchDir(stateDir, "relay")
	if err != nil {
		return err
	}
	if stateDir == "" {
		defer os.RemoveAll(scratch)
	}

	sides := []string{"compressed", "plain"}
	runs := make([]relayRun, len(sides))
	for i, side := range sides {
		if side == "plain" {
			if err := os.Setenv("OHYA_UPLOAD_COMPRESSION", "off"); err != nil {
				return err
			}
		}
		runs[i], err = relayDrain(side, filepath.Join(scratch, side), byAgent, total, mbits, rttMS)
		if err != nil {
			return err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "relay-shaped upload drain (in-process endpoint, shared shaped uplink)\n")
	fmt.Fprintf(&b, "corpus: %s, %d files, %.1f MiB; uplink %.1f Mbit/s shared by %d agent connections, %.0f ms rtt\n",
		root, len(files), float64(total)/(1<<20), mbits, len(byAgent), rttMS)
	fmt.Fprintf(&b, "platform: %s/%s, %d CPUs\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fmt.Fprintf(&b, "dependency: %s\n\n", depLine())
	fmt.Fprintf(&b, "%-11s %10s %9s %12s %12s %8s %9s\n",
		"side", "wall", "MiB/s", "payload", "wire", "ratio", "cpu s")
	for _, r := range runs {
		ratio := 0.0
		if r.wire > 0 {
			ratio = float64(r.payload) / float64(r.wire)
		}
		fmt.Fprintf(&b, "%-11s %10s %9.2f %12d %12d %8.2f %9.1f\n",
			r.name, r.wall.Round(time.Second), float64(total)/(1<<20)/r.wall.Seconds(),
			r.payload, r.wire, ratio, r.cpuSec)
	}
	for _, r := range runs {
		for _, line := range r.logLines {
			fmt.Fprintf(&b, "%s: %s\n", r.name, line)
		}
	}
	bts := float64(total) / (1 << 30)
	plain, comp := runs[1], runs[0]
	fmt.Fprintf(&b, "\nextrapolation to the measured 4.46 GB first join at this link speed:\n")
	fmt.Fprintf(&b, "plain:      %.1f h (%.1f Mbit/s achieved)\n",
		plain.wall.Hours()*4.46/bts, float64(total)*8/plain.wall.Seconds()/1e6)
	fmt.Fprintf(&b, "compressed: %.1f h (ratio %.2f on this corpus)\n",
		comp.wall.Hours()*4.46/bts, float64(comp.payload)/float64(comp.wire))
	fmt.Fprintf(&b, "elapsed: %s\n", time.Since(started).Round(time.Second))

	text := b.String()
	fmt.Print(text)
	return writeFile(out, text)
}

func relayDrain(side, dir string, byAgent map[string][]corpusFile, total int64, mbits, rttMS float64) (relayRun, error) {
	r := relayRun{name: side}
	stateDir := filepath.Join(dir, "device")
	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		return r, err
	}
	defer reg.Close()
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		return r, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inv, err := reg.Issue(ctx, time.Hour)
	if err != nil {
		return r, err
	}
	paired, err := reg.Consume(ctx, inv.Secret, deviceID)
	if err != nil {
		return r, err
	}
	cred := device.Credential{DeviceID: paired.DeviceID, Secret: paired.Secret}

	hub, err := store.Open(context.Background(), filepath.Join(dir, "hub.db"))
	if err != nil {
		return r, err
	}
	ep, err := hubsync.NewEndpoint(hub, &registry.Authenticator{DB: reg, PollInterval: 20 * time.Millisecond}, hubsync.Options{
		Retry: hubsync.RetryPolicy{Base: time.Millisecond},
	})
	if err != nil {
		return r, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return r, err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- ep.Serve(ctx, ln) }()

	bucket := newBucket(mbits * 1e6 / 8)
	defer bucket.close()
	delay := time.Duration(rttMS / 2 * float64(time.Millisecond))
	dial := func(ctx context.Context) (net.Conn, error) {
		var nd net.Dialer
		c, err := nd.DialContext(ctx, "tcp", ln.Addr().String())
		if err != nil {
			return nil, err
		}
		return &shapedConn{Conn: c, bucket: bucket, delay: delay}, nil
	}

	obs := observertest.New()
	logBuf := &syncBuffer{}
	var wg sync.WaitGroup
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	cpuBefore := processCPUSec()
	drainStart := time.Now()
	for agent, agentFiles := range byAgent {
		candidates := make([]string, len(agentFiles))
		for i, f := range agentFiles {
			candidates[i] = f.path
		}
		w, err := watch.New(stateDir, agent, watch.Options{Observer: obs})
		if err != nil {
			return r, err
		}
		up, err := uploader.New(w, cred, dial, uploader.Options{
			Agent:          agent,
			Interval:       250 * time.Millisecond,
			RequestTimeout: 60 * time.Second,
			Log:            logBuf,
			Observer:       obs,
		})
		if err != nil {
			return r, err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = up.Run(runCtx, func() ([]string, error) { return candidates, nil })
		}()
	}

	deadline := drainStart.Add(time.Duration(float64(total)/(mbits*1e6/8)*4)*time.Second + 5*time.Minute)
	complete := func() bool {
		for agent, agentFiles := range byAgent {
			snaps, err := watch.InspectSources(stateDir, agent)
			if err != nil {
				return false
			}
			byPath := map[string]int64{}
			for _, s := range snaps {
				byPath[s.Path] = s.CursorBytes
			}
			for _, f := range agentFiles {
				if byPath[f.path] != f.size {
					return false
				}
			}
		}
		return true
	}
	for {
		if complete() {
			break
		}
		if time.Now().After(deadline) {
			runCancel()
			wg.Wait()
			cancel()
			hub.Close()
			<-serveDone
			return r, fmt.Errorf("%s drain did not complete within %s; device log:\n%s", side, time.Since(drainStart), logBuf.String())
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return r, ctx.Err()
		}
	}
	r.wall = time.Since(drainStart)
	time.Sleep(500 * time.Millisecond)
	runCancel()
	wg.Wait()
	r.payload = obs.Counter("upload_payload_bytes")
	r.wire = obs.Counter("upload_wire_bytes")
	r.cpuSec = processCPUSec() - cpuBefore
	r.logLines = backlogLines(logBuf.String())

	events, err := countEvents(filepath.Join(dir, "hub.db"))
	if err != nil {
		return r, err
	}
	r.events = events
	if err := hub.Close(); err != nil {
		return r, err
	}
	cancel()
	if err := <-serveDone; err != nil && ctx.Err() == nil {
		return r, err
	}
	fmt.Fprintf(os.Stderr, "%s: wall=%s events=%d payload=%d wire=%d cpu=%.1fs\n",
		side, r.wall.Round(time.Second), events, r.payload, r.wire, r.cpuSec)
	return r, nil
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func backlogLines(log string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "upload backlog for") && (strings.Contains(line, "complete") || strings.Contains(line, "bytes/s")) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

type bucket struct {
	mu     sync.Mutex
	cond   *sync.Cond
	tokens float64
	rate   float64
	cap    float64
	stop   chan struct{}
	done   chan struct{}
}

func newBucket(rate float64) *bucket {
	b := &bucket{rate: rate, cap: rate * 0.05, stop: make(chan struct{}), done: make(chan struct{})}
	b.cond = sync.NewCond(&b.mu)
	go func() {
		defer close(b.done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-tick.C:
			}
			b.mu.Lock()
			b.tokens = min(b.cap, b.tokens+b.rate*0.005)
			b.mu.Unlock()
			b.cond.Broadcast()
		}
	}()
	return b
}

func (b *bucket) close() {
	close(b.stop)
	<-b.done
}

func (b *bucket) take(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for n > 0 {
		chunk := min(n, 1<<14)
		for b.tokens < float64(chunk) {
			b.cond.Wait()
		}
		b.tokens -= float64(chunk)
		n -= chunk
	}
}

type shapedConn struct {
	net.Conn
	bucket *bucket
	delay  time.Duration
}

func (c *shapedConn) Write(p []byte) (int, error) {
	c.bucket.take(len(p))
	time.Sleep(c.delay)
	return c.Conn.Write(p)
}

func countEvents(dbPath string) (int, error) {
	db, err := sql.Open("sqlite3", hubdata.ReadOnlyDSN(dbPath))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var events int
	if err := db.QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		return 0, err
	}
	return events, nil
}

func processCPUSec() float64 {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 15 {
		return 0
	}
	utime, err1 := strconv.ParseFloat(fields[13], 64)
	stime, err2 := strconv.ParseFloat(fields[14], 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	return (utime + stime) / 100.0
}

type dictFile struct {
	path    string
	section []byte
}

func runDict(root, dbPath string, dictKB int, maxTotal int64, out string, started time.Time) error {
	scratch, err := scratchDir("", "dict")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	var perAgent map[string][]dictFile
	switch {
	case dbPath != "":
		perAgent, err = sectionsFromHub(dbPath, maxTotal)
		if err != nil {
			return err
		}
	case root != "":
		perAgent, err = sectionsFromCorpus(root)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("-mode dict needs -root or -db")
	}

	type agentResult struct {
		name     string
		files    int
		raw      int64
		measure  int64
		levels   map[string]levelStat
		plain    int64
		withDict int64
		smallN   int
		smallRaw int64
		smallP   int64
		smallD   int64
	}
	var results []agentResult
	levels := []struct {
		name  string
		level int
	}{{"fastest", 1}, {"default", 3}, {"better", 7}, {"best", 11}}

	for _, agent := range agentDirs {
		files := perAgent[agent]
		if len(files) < 4 {
			continue
		}
		sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
		trainFiles, measureFiles := splitFiles(files)
		res := agentResult{name: agent, files: len(files), levels: map[string]levelStat{}}
		for _, f := range files {
			res.raw += int64(len(f.section))
		}
		plainEnc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)), zstd.WithEncoderCRC(true))
		if err != nil {
			return err
		}
		var plainBytes int64
		for _, f := range measureFiles {
			plainBytes += int64(len(plainEnc.EncodeAll(f.section, nil)))
		}
		plainEnc.Close()
		res.plain = plainBytes
		for _, f := range measureFiles {
			res.measure += int64(len(f.section))
		}

		for _, lv := range levels {
			st, err := measureLevel(lv.name, lv.level, measureFiles)
			if err != nil {
				return err
			}
			res.levels[lv.name] = st
		}

		var trainSamples [][]byte
		for _, f := range trainFiles {
			trainSamples = append(trainSamples, chunk(f.section, 64<<10)...)
		}
		trainSamples = hubdata.DictSamples(trainSamples, 4096, 64<<10)
		dict, err := hubdata.TrainDictionary(filepath.Join(scratch, agent), trainSamples, dictKB*1024)
		if err != nil {
			return fmt.Errorf("train dictionary for %s: %w", agent, err)
		}
		dictEnc, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
			zstd.WithEncoderCRC(true),
			zstd.WithEncoderDict(dict))
		if err != nil {
			return err
		}
		var dictBytes int64
		for _, f := range measureFiles {
			dictBytes += int64(len(dictEnc.EncodeAll(f.section, nil)))
		}
		dictEnc.Close()
		res.withDict = dictBytes

		var smallEnc, smallDict *zstd.Encoder
		smallEnc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)), zstd.WithEncoderCRC(true))
		if err != nil {
			return err
		}
		smallDict, err = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
			zstd.WithEncoderCRC(true),
			zstd.WithEncoderDict(dict))
		if err != nil {
			return err
		}
		for _, f := range measureFiles {
			if len(f.section) >= 128<<10 {
				continue
			}
			res.smallN++
			res.smallRaw += int64(len(f.section))
			res.smallP += int64(len(smallEnc.EncodeAll(f.section, nil)))
			res.smallD += int64(len(smallDict.EncodeAll(f.section, nil)))
		}
		smallEnc.Close()
		smallDict.Close()

		dec, err := zstd.NewReader(nil, zstd.WithDecoderDicts(dict))
		if err != nil {
			return err
		}
		probe := dictEnc.EncodeAll(measureFiles[0].section, nil)
		back, err := dec.DecodeAll(probe, nil)
		if err != nil || !bytes.Equal(back, measureFiles[0].section) {
			return fmt.Errorf("dictionary round trip failed for %s", agent)
		}
		dec.Close()
		results = append(results, res)
	}

	source := "corpus " + root
	if dbPath != "" {
		source = "hub database " + dbPath
	}
	var b strings.Builder
	fmt.Fprintf(&b, "zstd level and dictionary evaluation on upload record sections\n")
	fmt.Fprintf(&b, "source: %s; dictionary %d KiB trained on the disjoint half of the files, zstd --train CLI\n", source, dictKB)
	fmt.Fprintf(&b, "platform: %s/%s, %d CPUs\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fmt.Fprintf(&b, "dependency: %s\n\n", depLine())
	fmt.Fprintf(&b, "%-11s %6s %10s %10s %10s %10s %10s %10s\n",
		"agent", "files", "raw B", "fastest", "default", "better", "best", "+dict(def)")
	for _, res := range results {
		fmt.Fprintf(&b, "%-11s %6d %10d %10.2f %10.2f %10.2f %10.2f %10.2f\n",
			res.name, res.files, res.raw,
			ratioOrZero(res.measure, res.levels["fastest"].comp), ratioOrZero(res.measure, res.levels["default"].comp),
			ratioOrZero(res.measure, res.levels["better"].comp), ratioOrZero(res.measure, res.levels["best"].comp),
			ratioOrZero(res.measure, res.withDict))
	}
	fmt.Fprintf(&b, "\nper-level encode and decode speed and byte-weighted ratios (measure half only)\n")
	fmt.Fprintf(&b, "%-11s %-8s %9s %9s %8s %12s\n", "agent", "level", "enc MB/s", "dec MB/s", "ratio", "wire bytes")
	for _, res := range results {
		for _, lv := range levels {
			st := res.levels[lv.name]
			fmt.Fprintf(&b, "%-11s %-8s %9.0f %9.0f %8.2f %12d\n",
				res.name, lv.name, st.encMBs, st.decMBs, st.ratio, st.comp)
		}
	}
	fmt.Fprintf(&b, "\ndictionary verdict per agent (default level, whole measure half)\n")
	for _, res := range results {
		fmt.Fprintf(&b, "%-11s plain %d B, with dictionary %d B, additional reduction %.1f%%",
			res.name, res.plain, res.withDict, 100*(1-float64(res.withDict)/float64(res.plain)))
		if res.smallN > 0 && res.smallP > 0 {
			fmt.Fprintf(&b, "; files under 128 KiB: n=%d additional reduction %.1f%%",
				res.smallN, 100*(1-float64(res.smallD)/float64(res.smallP)))
		}
		fmt.Fprintf(&b, "\n")
	}
	fmt.Fprintf(&b, "elapsed: %s\n", time.Since(started).Round(time.Second))

	text := b.String()
	fmt.Print(text)
	return writeFile(out, text)
}

type levelStat struct {
	comp   int64
	encMBs float64
	decMBs float64
	ratio  float64
}

func measureLevel(name string, level int, files []dictFile) (levelStat, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)), zstd.WithEncoderCRC(true))
	if err != nil {
		return levelStat{}, err
	}
	defer enc.Close()
	var comp int64
	var raw int64
	streams := make([][]byte, len(files))
	start := time.Now()
	for i, f := range files {
		streams[i] = enc.EncodeAll(f.section, nil)
		comp += int64(len(streams[i]))
		raw += int64(len(f.section))
	}
	encodeWall := time.Since(start)
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return levelStat{}, err
	}
	defer dec.Close()
	dstart := time.Now()
	for i, f := range files {
		back, err := dec.DecodeAll(streams[i], make([]byte, 0, len(f.section)))
		if err != nil || !bytes.Equal(back, f.section) {
			return levelStat{}, fmt.Errorf("level %s round trip failed", name)
		}
	}
	decodeWall := time.Since(dstart)
	st := levelStat{comp: comp, ratio: float64(raw) / float64(comp)}
	if encodeWall > 0 {
		st.encMBs = float64(raw) / 1e6 / encodeWall.Seconds()
	}
	if decodeWall > 0 {
		st.decMBs = float64(raw) / 1e6 / decodeWall.Seconds()
	}
	return st, nil
}

func ratioOrZero(raw, comp int64) float64 {
	if comp == 0 {
		return 0
	}
	return float64(raw) / float64(comp)
}

func splitFiles(files []dictFile) (train, measure []dictFile) {
	var total int64
	for _, f := range files {
		total += int64(len(f.section))
	}
	var acc int64
	for i, f := range files {
		acc += int64(len(f.section))
		if acc >= total/2 {
			rest := make([]dictFile, len(files)-i-1)
			copy(rest, files[i+1:])
			return files[: i+1 : i+1], rest
		}
	}
	return files, nil
}

func chunk(data []byte, size int) [][]byte {
	var out [][]byte
	for off := 0; off < len(data); off += size {
		end := min(off+size, len(data))
		out = append(out, data[off:end:end])
	}
	return out
}

func sectionFromRecords(records [][]byte) []byte {
	var section []byte
	var off int64
	for _, rec := range records {
		var head [12]byte
		binary.BigEndian.PutUint64(head[:8], uint64(off))
		binary.BigEndian.PutUint32(head[8:], uint32(len(rec)))
		section = append(section, head[:]...)
		section = append(section, rec...)
		off += int64(len(rec)) + 1
	}
	return section
}

func sectionsFromCorpus(root string) (map[string][]dictFile, error) {
	corpus, _, err := walkCorpus(root)
	if err != nil {
		return nil, err
	}
	out := map[string][]dictFile{}
	for _, f := range corpus {
		data, err := os.ReadFile(f.path)
		if err != nil {
			return nil, err
		}
		out[f.agent] = append(out[f.agent], dictFile{path: f.path, section: fileSection(data)})
	}
	return out, nil
}

func sectionsFromHub(dbPath string, maxTotal int64) (map[string][]dictFile, error) {
	hub, err := hubdata.OpenReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer hub.Close()
	out := map[string][]dictFile{}
	for _, agent := range agentDirs {
		count, err := hubdata.CountAgentSessions(hub, agent)
		if err != nil {
			return nil, err
		}
		if count == 0 {
			continue
		}
		stride := max(1, count/512)
		sessions, err := hubdata.SampleAgentSessions(hub, agent, stride)
		if err != nil {
			return nil, err
		}
		var read int64
		for _, s := range sessions {
			if read >= maxTotal {
				break
			}
			records, got, err := hubdata.ReadSessionRaw(hub, s.ID, 64<<20)
			if err != nil {
				return nil, err
			}
			if got == 0 {
				continue
			}
			read += got
			path := fmt.Sprintf("%s/%s", agent, s.ID)
			out[agent] = append(out[agent], dictFile{path: path, section: sectionFromRecords(records)})
		}
		fmt.Fprintf(os.Stderr, "%s: %d of %d sessions sampled (stride %d), %.1f MB read\n",
			agent, len(out[agent]), count, stride, float64(read)/1e6)
	}
	return out, nil
}

func fileSection(data []byte) []byte {
	var records [][]byte
	off := 0
	for off < len(data) {
		idx := bytes.IndexByte(data[off:], '\n')
		if idx < 0 {
			break
		}
		records = append(records, data[off:off+idx:off+idx])
		off += idx + 1
	}
	return sectionFromRecords(records)
}

func scratchDir(state, name string) (string, error) {
	if state != "" {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return "", err
		}
		return state, nil
	}
	prefix := os.Getenv("LOOPER_RESOURCE_PREFIX")
	if prefix == "" {
		prefix = "ohya"
	}
	return os.MkdirTemp("", prefix+"-uploadbench-"+name+"-")
}

func writeFile(out, text string) error {
	if out == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, []byte(text), 0o644)
}

func depLine() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "klauspost/compress (version unknown)"
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/klauspost/compress" {
			return fmt.Sprintf("github.com/klauspost/compress %s, %s", dep.Version, info.GoVersion)
		}
	}
	return fmt.Sprintf("klauspost/compress (not in build info), %s", info.GoVersion)
}
