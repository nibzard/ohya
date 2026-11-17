package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/nibzard/ohya/perf/hubdata"
)

type rowStats struct {
	rows      int64
	rawBytes  int64
	compBytes int64
	dup       hubdata.DupTotals
	globalDup int64
}

func (r *rowStats) ratio() float64 {
	if r.compBytes == 0 {
		return 0
	}
	return float64(r.rawBytes) / float64(r.compBytes)
}

type deviceResult struct {
	id    string
	label string
	rows  rowStats
	sess  sessStats
}

type sessStats struct {
	bytesRead    int64
	dup          hubdata.DupTotals
	rowDictSelf  ratioPair
	rowDictOther ratioPair
	block64      ratioPair
	block1M      ratioPair
	block64Dict  ratioPair
	block1MDict  ratioPair
	dictSize     int
	dictTrain    int64
	dictMeasure  int64
	sessions     int64
}

type ratioPair struct {
	raw  int64
	comp int64
}

func (p ratioPair) ratio() float64 {
	if p.comp == 0 {
		return 0
	}
	return float64(p.raw) / float64(p.comp)
}

func main() {
	dbPath := flag.String("db", filepath.Join(homeDir(), ".ohya", "hub.db"), "hub database (opened read-only)")
	sample := flag.Int("sample", 20, "phase A: take every Nth event id")
	draws := flag.Int("draws", 60, "phase B: sessions per device, probability proportional to size")
	maxSession := flag.Int64("max-session-bytes", 256<<20, "phase B: skip sessions estimated above this many bytes")
	maxTotal := flag.Int64("max-total-bytes", 900<<20, "phase B: read at most this many raw bytes per device")
	dictSize := flag.Int("dict-size", 112640, "trained dictionary size in bytes (110 KB)")
	scratch := flag.String("scratch", "", "scratch directory for dictionaries (default: fresh temp dir)")
	out := flag.String("out", "perf/t102/device-storage.txt", "output file (aggregate numbers only)")
	flag.Parse()

	started := time.Now()
	if err := run(*dbPath, *sample, *draws, *maxSession, *maxTotal, int64(*dictSize), *scratch, *out, started); err != nil {
		fmt.Fprintln(os.Stderr, "devratio:", err)
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

type sessionSample struct {
	count int64
	bytes int64
}

func run(dbPath string, sample, draws int, maxSession, maxTotal, dictSize int64, scratch, out string, started time.Time) error {
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
		scratch, err = os.MkdirTemp("", prefix+"-devratio-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(scratch)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return err
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	defer enc.Close()

	sessions, err := hubdata.AllSessions(hub)
	if err != nil {
		return err
	}
	sessIndex := map[string]hubdata.Session{}
	for _, s := range sessions {
		sessIndex[s.ID] = s
	}

	perDevice := map[string]*rowStats{}
	sessSamples := map[string]*sessionSample{}
	seenPerDevice := map[string]map[[16]byte]struct{}{}
	seenGlobal := map[[16]byte]struct{}{}

	err = phaseA(hub, enc, sample, perDevice, sessSamples, seenPerDevice, seenGlobal)
	if err != nil {
		return err
	}

	labels, err := deviceLabels(hub)
	if err != nil {
		return err
	}

	results := map[string]*deviceResult{}
	for deviceID, stats := range perDevice {
		res := &deviceResult{id: deviceID, label: labels[deviceID], rows: *stats}
		results[deviceID] = res
	}

	order := make([]string, 0, len(results))
	for id := range results {
		order = append(order, id)
	}
	sort.Strings(order)

	for _, deviceID := range order {
		res := results[deviceID]
		err = phaseB(hub, scratch, deviceID, res, sessIndex, sessSamples, draws, maxSession, maxTotal, dictSize)
		if err != nil {
			return fmt.Errorf("device %s: %w", deviceID, err)
		}
		fmt.Fprintf(os.Stderr, "device %s (%s): phase B done, %d sessions, %.1f MB read\n",
			shortID(deviceID), res.label, res.sess.sessions, float64(res.sess.bytesRead)/1e6)
	}

	report(out, sample, draws, order, results, started)
	return nil
}

func phaseA(hub *sql.DB, enc *zstd.Encoder, sample int, perDevice map[string]*rowStats,
	sessSamples map[string]*sessionSample, seenPerDevice map[string]map[[16]byte]struct{},
	seenGlobal map[[16]byte]struct{}) error {

	rows, err := hub.Query(`
		SELECT e.session_id, e.raw_json, s.device_id
		FROM events e JOIN sessions s ON s.id = e.session_id
		WHERE e.id % ? = 0`, sample)
	if err != nil {
		return err
	}
	defer rows.Close()

	var scanned int64
	var lastReport int64
	for rows.Next() {
		var sessionID, raw, deviceID string
		if err := rows.Scan(&sessionID, &raw, &deviceID); err != nil {
			return err
		}
		scanned++
		stats := perDevice[deviceID]
		if stats == nil {
			stats = &rowStats{}
			perDevice[deviceID] = stats
			seenPerDevice[deviceID] = map[[16]byte]struct{}{}
		}
		stats.rows++
		stats.rawBytes += int64(len(raw))
		stats.compBytes += int64(len(enc.EncodeAll([]byte(raw), nil)))

		ss := sessSamples[sessionID]
		if ss == nil {
			ss = &sessionSample{}
			sessSamples[sessionID] = ss
		}
		ss.count++
		ss.bytes += int64(len(raw))

		rawBytes := []byte(raw)
		stats.dup.RawBytes += int64(len(rawBytes))
		hubdata.VisitLargeStrings(rawBytes, 256, func(s string) {
			n := int64(len(s))
			stats.dup.StringBytes += n
			key := hubdata.HashKey(s)
			_, devDup := seenPerDevice[deviceID][key]
			_, anyDup := seenGlobal[key]
			if devDup {
				stats.dup.RepeatBytes += n
			}
			if anyDup && !devDup {
				stats.globalDup += n
			}
			seenPerDevice[deviceID][key] = struct{}{}
			seenGlobal[key] = struct{}{}
		})

		if scanned-lastReport >= 20000 {
			lastReport = scanned
			fmt.Fprintf(os.Stderr, "phase A: %d sampled events\n", scanned)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "phase A: %d sampled events in total\n", scanned)
	return nil
}

func deviceLabels(hub *sql.DB) (map[string]string, error) {
	rows, err := hub.Query(`SELECT device_id, min(path) FROM sources GROUP BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[string]string{}
	for rows.Next() {
		var deviceID, path string
		if err := rows.Scan(&deviceID, &path); err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(path, "/Users/"):
			labels[deviceID] = "macbook"
		case strings.HasPrefix(path, "/home/"):
			labels[deviceID] = "vm"
		default:
			labels[deviceID] = "other"
		}
	}
	return labels, rows.Err()
}

func phaseB(hub *sql.DB, scratch, deviceID string, res *deviceResult, sessIndex map[string]hubdata.Session,
	sessSamples map[string]*sessionSample, draws int, maxSession, maxTotal, dictSize int64) error {

	stats := res.rows
	var deviceMean float64
	if stats.rows > 0 {
		deviceMean = float64(stats.rawBytes) / float64(stats.rows)
	}

	type est struct {
		id   string
		size int64
	}
	var ests []est
	var totalEst int64
	for _, s := range sessIndex {
		if s.DeviceID != deviceID {
			continue
		}
		mean := deviceMean
		if ss := sessSamples[s.ID]; ss != nil && ss.count >= 3 {
			mean = float64(ss.bytes) / float64(ss.count)
		}
		size := int64(float64(s.EventCount) * mean)
		if size <= 0 || size > maxSession {
			continue
		}
		ests = append(ests, est{s.ID, size})
		totalEst += size
	}
	if len(ests) == 0 || totalEst == 0 {
		return fmt.Errorf("no sessions to sample")
	}
	sort.Slice(ests, func(i, j int) bool { return ests[i].id < ests[j].id })

	var picked []est
	var cum int64
	var pickedBytes int64
	pos := 0
	step := float64(totalEst) / float64(draws)
	for d := 0; d < draws && pos < len(ests); d++ {
		target := float64(d)*step + step/2
		for pos < len(ests) && float64(cum+ests[pos].size) < target {
			cum += ests[pos].size
			pos++
		}
		if pos >= len(ests) {
			break
		}
		pick := ests[pos]
		if pickedBytes+pick.size > maxTotal {
			break
		}
		already := false
		for _, p := range picked {
			if p.id == pick.id {
				already = true
				break
			}
		}
		if already {
			continue
		}
		picked = append(picked, pick)
		pickedBytes += pick.size
	}
	if len(picked) == 0 {
		return fmt.Errorf("no sessions picked")
	}

	encPlain, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	defer encPlain.Close()

	var records [][]byte
	dupMap := map[[16]byte]struct{}{}
	dup := hubdata.DupTotals{}
	for _, pick := range picked {
		recs, _, err := hubdata.ReadSessionRaw(hub, pick.id, maxSession)
		if err != nil {
			return err
		}
		for _, r := range recs {
			hubdata.WalkLargeStrings(r, 256, dupMap, &dup)
		}
		records = append(records, recs...)
	}
	res.sess.sessions = int64(len(picked))
	res.sess.bytesRead = dup.RawBytes
	res.sess.dup = dup

	blocks64 := hubdata.BuildBlocks(records, 64<<10)
	blocks1M := hubdata.BuildBlocks(records, 1<<20)
	res.sess.block64 = blockRatio(encPlain, blocks64)
	res.sess.block1M = blockRatio(encPlain, blocks1M)

	train, measure := hubdata.SplitHalves(records)
	samples := hubdata.DictSamples(train, 4096, 64<<10)
	dict, err := hubdata.TrainDictionary(filepath.Join(scratch, "dict-"+shortID(deviceID)), samples, int(dictSize))
	if err != nil {
		return fmt.Errorf("train dictionary: %w", err)
	}
	res.sess.dictSize = len(dict)

	encDict, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderDict(dict))
	if err != nil {
		return err
	}
	defer encDict.Close()

	res.sess.rowDictSelf = rowRatio(encDict, train)
	res.sess.rowDictOther = rowRatio(encDict, measure)
	res.sess.block64Dict = blockRatio(encDict, hubdata.BuildBlocks(measure, 64<<10))
	res.sess.block1MDict = blockRatio(encDict, hubdata.BuildBlocks(measure, 1<<20))
	for _, r := range train {
		res.sess.dictTrain += int64(len(r))
	}
	for _, r := range measure {
		res.sess.dictMeasure += int64(len(r))
	}
	return nil
}

func rowRatio(enc *zstd.Encoder, records [][]byte) ratioPair {
	var p ratioPair
	for _, r := range records {
		p.raw += int64(len(r))
		p.comp += int64(len(enc.EncodeAll(r, nil)))
	}
	return p
}

func blockRatio(enc *zstd.Encoder, blocks [][]byte) ratioPair {
	var p ratioPair
	for _, b := range blocks {
		p.raw += int64(len(b))
		p.comp += int64(len(enc.EncodeAll(b, nil)))
	}
	return p
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func report(out string, sample, draws int, order []string, results map[string]*deviceResult, started time.Time) {
	var b strings.Builder
	fmt.Fprintf(&b, "Duplication and compression ratios per device\n")
	fmt.Fprintf(&b, "phase A: every %dth event id, per-row zstd SpeedDefault (level 3), strings >= 256 bytes\n", sample)
	fmt.Fprintf(&b, "phase B: %d sessions per device, probability proportional to estimated size\n", draws)
	fmt.Fprintf(&b, "dictionary: zstd --train on the train half of the picked sessions\n\n")

	for _, deviceID := range order {
		res := results[deviceID]
		fmt.Fprintf(&b, "device %s (%s)\n", shortID(deviceID), res.label)
		fmt.Fprintf(&b, "  phase A sampled rows: %d, raw %.1f MB\n", res.rows.rows, float64(res.rows.rawBytes)/1e6)
		fmt.Fprintf(&b, "  per-row zstd-3 ratio (no dict): %.2f\n", res.rows.ratio())
		fmt.Fprintf(&b, "  large strings (>=256 B) share of raw bytes: %.1f%%\n", res.rows.dup.LargeStringShare()*100)
		fmt.Fprintf(&b, "  repeat share of raw bytes (within device): %.1f%%\n", res.rows.dup.RepeatShare()*100)
		global := 0.0
		if res.rows.rawBytes > 0 {
			global = float64(res.rows.globalDup) / float64(res.rows.rawBytes)
		}
		fmt.Fprintf(&b, "  repeat share of raw bytes (across devices): %.1f%%\n", global*100)
		fmt.Fprintf(&b, "  phase B sessions: %d, raw read %.1f MB\n", res.sess.sessions, float64(res.sess.bytesRead)/1e6)
		fmt.Fprintf(&b, "  session-stream repeat share: %.1f%%\n", res.sess.dup.RepeatShare()*100)
		fmt.Fprintf(&b, "  blocks 64 KB zstd-3 (no dict): %.2f\n", res.sess.block64.ratio())
		fmt.Fprintf(&b, "  blocks 1 MB zstd-3 (no dict): %.2f\n", res.sess.block1M.ratio())
		fmt.Fprintf(&b, "  dictionary: %d bytes, train half %.1f MB, measure half %.1f MB\n",
			res.sess.dictSize, float64(res.sess.dictTrain)/1e6, float64(res.sess.dictMeasure)/1e6)
		fmt.Fprintf(&b, "  per-row zstd-3 with dict (train half, self): %.2f\n", res.sess.rowDictSelf.ratio())
		fmt.Fprintf(&b, "  per-row zstd-3 with dict (measure half): %.2f\n", res.sess.rowDictOther.ratio())
		fmt.Fprintf(&b, "  blocks 64 KB zstd-3 with dict (measure half): %.2f\n", res.sess.block64Dict.ratio())
		fmt.Fprintf(&b, "  blocks 1 MB zstd-3 with dict (measure half): %.2f\n", res.sess.block1MDict.ratio())
		fmt.Fprintf(&b, "\n")
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
