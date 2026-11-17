package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/nibzard/ohya/perf/hubdata"
)

type config struct {
	name    string
	block   int
	withDic bool
}

type result struct {
	cfg      config
	blocks   int
	rawByte  int64
	compByt  int64
	passes   int
	decNs    int64
	meanSize float64
}

func (r result) mibs() float64 {
	return float64(r.rawByte) / 1e6
}

func (r result) ratio() float64 {
	if r.compByt == 0 {
		return 0
	}
	return float64(r.rawByte) / float64(r.compByt)
}

func (r result) decMBs() float64 {
	if r.decNs == 0 {
		return 0
	}
	return float64(r.rawByte) * float64(r.passes) / 1e6 / (float64(r.decNs) / 1e9)
}

func (r result) usPerBlock() float64 {
	if r.blocks == 0 {
		return 0
	}
	return float64(r.decNs) / float64(r.blocks*r.passes) / 1e3
}

func main() {
	dbPath := flag.String("db", filepath.Join(homeDir(), ".ohya", "hub.db"), "hub database (opened read-only)")
	sessions := flag.Int("sessions", 40, "number of largest sessions to read")
	maxSession := flag.Int64("max-session-bytes", 64<<20, "skip sessions above this many raw bytes")
	maxTotal := flag.Int64("max-total-bytes", 128<<20, "read at most this many raw bytes in total")
	dictSize := flag.Int("dict-size", 112640, "trained dictionary size in bytes (110 KB)")
	seconds := flag.Float64("seconds", 5, "decode timing window per configuration")
	scratch := flag.String("scratch", "", "scratch directory for dictionaries (default: fresh temp dir)")
	out := flag.String("out", "perf/t102/zstd-decode.txt", "output file (aggregate numbers only)")
	flag.Parse()

	started := time.Now()
	if err := run(*dbPath, *sessions, *maxSession, *maxTotal, *dictSize, *seconds, *scratch, *out, started); err != nil {
		fmt.Fprintln(os.Stderr, "zstdbench:", err)
		os.Exit(1)
	}
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

func run(dbPath string, sessionCount int, maxSession, maxTotal int64, dictSize int, seconds float64, scratch, out string, started time.Time) error {
	hub, err := hubdata.OpenReadOnly(dbPath)
	if err != nil {
		return err
	}
	defer hub.Close()

	if scratch == "" {
		prefix := os.Getenv("LOOPER_RESOURCE_PREFIX")
		if prefix == "" {
			prefix = "ohya"
		}
		scratch, err = os.MkdirTemp("", prefix+"-zstdbench-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(scratch)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return err
	}

	big, err := hubdata.LargestSessions(hub, sessionCount*4)
	if err != nil {
		return err
	}
	var records [][]byte
	var read int64
	used := 0
	for _, s := range big {
		if used >= sessionCount || read >= maxTotal {
			break
		}
		if int64(s.EventCount)*4096 > maxSession {
			continue
		}
		recs, got, err := hubdata.ReadSessionRaw(hub, s.ID, maxSession)
		if err != nil {
			return err
		}
		if got == 0 {
			continue
		}
		used++
		read += got
		records = append(records, recs...)
	}
	if len(records) == 0 {
		return fmt.Errorf("no records read")
	}
	fmt.Fprintf(os.Stderr, "loaded %d sessions, %d records, %.1f MB raw\n", used, len(records), float64(read)/1e6)

	train, measure := hubdata.SplitHalves(records)
	samples := hubdata.DictSamples(train, 4096, 64<<10)
	dict, err := hubdata.TrainDictionary(filepath.Join(scratch, "dict"), samples, dictSize)
	if err != nil {
		return fmt.Errorf("train dictionary: %w", err)
	}
	fmt.Fprintf(os.Stderr, "trained dictionary: %d bytes (train half %.1f MB, measure half %.1f MB)\n",
		len(dict), float64(sumLen(train))/1e6, float64(sumLen(measure))/1e6)

	configs := []config{
		{"64 KB block, no dictionary", 64 << 10, false},
		{"64 KB block, dictionary", 64 << 10, true},
		{"1 MB block, no dictionary", 1 << 20, false},
		{"1 MB block, dictionary", 1 << 20, true},
	}

	var results []result
	for _, cfg := range configs {
		r, err := benchConfig(cfg, dict, measure, seconds)
		if err != nil {
			return err
		}
		results = append(results, r)
		fmt.Fprintf(os.Stderr, "%s: %.0f MB/s, %.1f us/block\n", cfg.name, r.decMBs(), r.usPerBlock())
	}
	report(out, results, used, len(dict), seconds, started)
	return nil
}

func sumLen(records [][]byte) int64 {
	var n int64
	for _, r := range records {
		n += int64(len(r))
	}
	return n
}

func benchConfig(cfg config, dict []byte, records [][]byte, seconds float64) (result, error) {
	r := result{cfg: cfg}
	var encOpts []zstd.EOption
	encOpts = append(encOpts, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if cfg.withDic {
		encOpts = append(encOpts, zstd.WithEncoderDict(dict))
	}
	enc, err := zstd.NewWriter(nil, encOpts...)
	if err != nil {
		return r, err
	}
	defer enc.Close()

	blocks := hubdata.BuildBlocks(records, cfg.block)
	r.blocks = len(blocks)
	if r.blocks > 0 {
		r.meanSize = float64(sumLen(blocks)) / float64(r.blocks)
	}
	type pair struct {
		raw  []byte
		comp []byte
	}
	pairs := make([]pair, 0, len(blocks))
	for _, b := range blocks {
		comp := enc.EncodeAll(b, nil)
		r.rawByte += int64(len(b))
		r.compByt += int64(len(comp))
		pairs = append(pairs, pair{b, comp})
	}

	var decOpts []zstd.DOption
	decOpts = append(decOpts, zstd.WithDecoderConcurrency(1))
	if cfg.withDic {
		decOpts = append(decOpts, zstd.WithDecoderDicts(dict))
	}
	dec, err := zstd.NewReader(nil, decOpts...)
	if err != nil {
		return r, err
	}
	defer dec.Close()

	for _, p := range pairs {
		back, err := dec.DecodeAll(p.comp, nil)
		if err != nil {
			return r, fmt.Errorf("%s round-trip: %w", cfg.name, err)
		}
		if !bytes.Equal(back, p.raw) {
			return r, fmt.Errorf("%s round-trip: bytes differ", cfg.name)
		}
	}

	bufs := make([][]byte, len(pairs))
	for i, p := range pairs {
		bufs[i] = make([]byte, 0, len(p.raw))
	}
	for i, p := range pairs {
		if _, err := dec.DecodeAll(p.comp, bufs[i][:0]); err != nil {
			return r, err
		}
	}

	minPasses := 3
	for {
		var passNs int64
		for i, p := range pairs {
			start := time.Now()
			if _, err := dec.DecodeAll(p.comp, bufs[i][:0]); err != nil {
				return r, err
			}
			passNs += time.Since(start).Nanoseconds()
		}
		r.decNs += passNs
		r.passes++
		if r.passes >= minPasses && float64(r.decNs) >= seconds*1e9 {
			break
		}
	}
	return r, nil
}

func report(out string, results []result, sessions, dictBytes int, seconds float64, started time.Time) {
	var b strings.Builder
	fmt.Fprintf(&b, "klauspost/compress zstd decode cost per block\n")
	fmt.Fprintf(&b, "blocks: consecutive raw_json records of the %d largest hub sessions, split at record boundaries\n", sessions)
	fmt.Fprintf(&b, "encode: SpeedDefault (level 3); dictionary trained with zstd --train on the other half of the data\n")
	fmt.Fprintf(&b, "decode: DecodeAll, single goroutine, warmed, %.0f s window, %d+ passes\n\n", seconds, minPassesOf(results))
	fmt.Fprintf(&b, "%s\n", platformLine())
	fmt.Fprintf(&b, "%s\n\n", depLine())

	fmt.Fprintf(&b, "%-28s %7s %9s %9s %8s %9s %11s %9s\n",
		"configuration", "blocks", "mean B", "raw MB", "ratio", "MB/s", "us/block", "ms/MB")
	for _, r := range results {
		fmt.Fprintf(&b, "%-28s %7d %9.0f %9.1f %8.2f %9.0f %11.1f %9.3f\n",
			r.cfg.name, r.blocks, r.meanSize, r.mibs(), r.ratio(), r.decMBs(), r.usPerBlock(),
			msPerMB(r))
	}
	fmt.Fprintf(&b, "elapsed: %s\n", time.Since(started).Round(time.Second))

	text := b.String()
	fmt.Print(text)
	if out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
			_ = os.WriteFile(out, []byte(text), 0o644)
		}
	}
}

func msPerMB(r result) float64 {
	if r.decNs == 0 || r.rawByte == 0 {
		return 0
	}
	return float64(r.decNs) / 1e6 / (float64(r.rawByte) / 1e6) / float64(r.passes)
}

func minPassesOf(results []result) int {
	min := 0
	for i, r := range results {
		if i == 0 || r.passes < min {
			min = r.passes
		}
	}
	return min
}

func platformLine() string {
	cpu := cpuModel()
	return fmt.Sprintf("platform: %s/%s, %d CPUs%s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), cpu)
}

func cpuModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") {
			if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
				return ", " + strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func depLine() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dependency: klauspost/compress (version unknown)"
	}
	goVer := info.GoVersion
	for _, dep := range info.Deps {
		if dep.Path == "github.com/klauspost/compress" {
			return fmt.Sprintf("dependency: github.com/klauspost/compress %s, %s", dep.Version, goVer)
		}
	}
	return fmt.Sprintf("dependency: klauspost/compress (not in build info), %s", goVer)
}
