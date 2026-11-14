// Package fixgen generates deterministic synthetic session corpora for
// benchmarks and performance tests: Generate and Write produce the Claude
// Code format, GenerateCodex and WriteCodex the Codex rollout format, and
// GeneratePi and WritePi the Pi agent session format. Each generator follows
// the verified format description of its agent (testdata/<agent>/README.md),
// scaled to configurable volumes, so measurements reproduce without real
// transcripts: the same Corpus value always generates the same bytes, with no
// clock or environment input.
//
// Every corpus is intentionally text-heavy with a skewed word distribution: a
// few common words appear in many records (wide FTS matches) and each marker
// word appears in a small fraction of records (selective FTS matches), which
// is what query benchmarks need to stay realistic. All three generators draw
// texts from the same distribution, so per-agent benchmarks differ in record
// envelope and parser, not in text volume.
package fixgen

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// Corpus describes one synthetic corpus. The zero value is not usable. The
// same knobs shape every agent's corpus; GenerateCodex applies them to
// rollout files (subagent rollouts included) and GeneratePi to one file per
// session, where SubagentsPerSession has no equivalent and stays unused.
type Corpus struct {
	// Sessions is the number of main session files.
	Sessions int
	// MainMeanBytes is the approximate size of one main session file; the
	// actual size varies per session and lands within one turn of it.
	MainMeanBytes int64
	// SubagentsPerSession is the mean number of subagent transcript files
	// per main session (each about a quarter of the main file's size).
	// Zero disables subagent files.
	SubagentsPerSession float64
	// Seed changes the corpus deterministically; 0 uses 1.
	Seed int64
}

// File is one generated session file: its path relative to the corpus root
// and its full JSONL bytes.
type File struct {
	Path string
	Data []byte
}

// Generate returns every file of the corpus in a deterministic order: for
// each session the main file first, then its subagent files. The result
// holds the whole corpus in memory; ingestion itself streams, so benchmark
// setups call this once outside the timed loop.
func Generate(c Corpus) []File {
	rng := rand.New(rand.NewPCG(uint64(c.seed()), 0x6f687961))
	var files []File
	for i := 0; i < c.Sessions; i++ {
		s := newSession(c, rng, i)
		files = append(files, File{Path: s.mainPath(), Data: s.mainJSON(c)})
		for sub := 0; sub < s.subagents; sub++ {
			files = append(files, File{Path: s.subPath(sub), Data: s.subJSON(c, sub)})
		}
	}
	return files
}

// Write writes the Claude Code corpus under root, mirroring the projects
// layout (<encoded-cwd>/<session>.jsonl plus subagents/), and returns the
// total bytes written. Directories are created as needed.
func Write(root string, c Corpus) (int64, error) {
	return writeFiles(root, Generate(c))
}

// writeFiles writes generated files under root in path order.
func writeFiles(root string, files []File) (int64, error) {
	var total int64
	for _, f := range files {
		p := filepath.Join(root, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return total, fmt.Errorf("fixgen: create %s: %w", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			return total, fmt.Errorf("fixgen: write %s: %w", p, err)
		}
		total += int64(len(f.Data))
	}
	return total, nil
}

// uuid formats a deterministic UUID-shaped identifier of the version-7 form
// the Codex and Pi formats write. hi and n only need to be unique per call.
func uuid(hi, n uint64) string {
	return fmt.Sprintf("%08x-0000-7000-8000-%012x", hi, n)
}

// compact serializes a generated JSON value; the inputs are maps and slices
// of primitives, so marshaling cannot fail.
func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("fixgen: marshal: " + err.Error())
	}
	return string(b)
}

func (c Corpus) seed() int64 {
	if c.Seed == 0 {
		return 1
	}
	return c.Seed
}

// sessionGen carries the per-session deterministic state shared by the main
// file and its subagent files.
type sessionGen struct {
	idx     int
	project string
	cwd     string
	branch  string
	id      string // session UUID; also the main file name
	seed    int64  // corpus seed, folded into the per-file generators
	// subagents is the number of subagent files drawn for this session.
	subagents int
	// baseMS is the session's first timestamp, offset per session so
	// sessions interleave in time like real usage.
	baseMS int64
}

func newSession(c Corpus, rng *rand.Rand, idx int) *sessionGen {
	proj := projects[idx%len(projects)]
	s := &sessionGen{
		idx:    idx,
		cwd:    "/home/agent/" + proj,
		branch: branches[idx%len(branches)],
		id:     fmt.Sprintf("%08x-0000-4000-8000-%012x", 0x5e550000+uint64(idx), uint64(idx)),
		seed:   c.seed(),
		baseMS: int64(idx) * 86_400_000 / 3, // sessions start eight hours apart
	}
	s.project = encodeCwd(s.cwd)
	if c.SubagentsPerSession > 0 {
		// Uniform draw around the mean, capped at 4.
		s.subagents = int(rng.Float64() * 2 * c.SubagentsPerSession)
		if s.subagents > 4 {
			s.subagents = 4
		}
	}
	return s
}

func (s *sessionGen) mainPath() string {
	return filepath.Join(s.project, s.id+".jsonl")
}

func (s *sessionGen) subPath(i int) string {
	return filepath.Join(s.project, s.id, "subagents", fmt.Sprintf("agent-%08x.jsonl", uint64(i)+1))
}

// encodeCwd applies Claude Code's lossy directory encoding: every '/', '.'
// and '_' becomes '-' (testdata/claude-code/README.md).
func encodeCwd(cwd string) string {
	out := make([]byte, len(cwd))
	for i := 0; i < len(cwd); i++ {
		switch cwd[i] {
		case '/', '.', '_':
			out[i] = '-'
		default:
			out[i] = cwd[i]
		}
	}
	return string(out)
}

// rec is one JSONL record under construction. encoding/json sorts map keys,
// so marshal output is deterministic.
type rec map[string]any

// sessionWriter emits one file's records as JSONL bytes.
type sessionWriter struct {
	s       *sessionGen
	textGen      // word and text distribution shared by every agent's generator
	sub     bool // subagent transcript
	buf     []byte
	uuid    int64
	ms      int64
}

func (w *sessionWriter) emit(r rec) {
	w.uuid++
	r["uuid"] = w.uuidString(w.uuid)
	r["parentUuid"] = w.uuidString(w.uuid - 1) // 0 renders as null
	w.ms += int64(20 + w.rng.IntN(4000))
	r["timestamp"] = isoMS(w.s.baseMS + w.ms)
	b, err := json.Marshal(r)
	if err != nil {
		panic("fixgen: marshal record: " + err.Error())
	}
	w.buf = append(w.buf, b...)
	w.buf = append(w.buf, '\n')
}

func (w *sessionWriter) uuidString(n int64) any {
	if n <= 0 {
		return nil
	}
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", 0xabcd0000, uint64(n))
}

// mainJSON generates one main session file: opening bookkeeping, a sequence
// of turns, closing bookkeeping.
func (s *sessionGen) mainJSON(c Corpus) []byte {
	w := &sessionWriter{s: s, textGen: textGen{rng: rand.New(rand.NewPCG(s.seed2(0), 0x6d61696e))}}
	w.emit(rec{"type": "queue-operation", "sessionId": s.id, "operation": "add"})
	w.emit(rec{"type": "mode", "sessionId": s.id, "mode": "default"})
	for int64(len(w.buf)) < c.MainMeanBytes {
		w.turn()
	}
	w.emit(rec{"type": "atis-latch", "sessionId": s.id, "latched": true})
	w.emit(rec{"type": "last-prompt", "sessionId": s.id, "leafUuid": w.uuidString(w.uuid)})
	w.emit(rec{"type": "ai-title", "sessionId": s.id, "title": w.sentence(3, 8)})
	w.emit(rec{"type": "cost-state", "sessionId": s.id,
		"totalCostUSD": 12.5, "totalDurationMs": 600_000, "modelUsage": map[string]any{
			"claude-opus-4-6": map[string]any{"inputTokens": 200000, "outputTokens": 40000},
		}})
	return w.buf
}

// subJSON generates one subagent transcript of about a quarter of the main
// file's size, reusing the parent session id with isSidechain and agentId,
// as verified in the real corpus.
func (s *sessionGen) subJSON(c Corpus, i int) []byte {
	w := &sessionWriter{s: s, textGen: textGen{rng: rand.New(rand.NewPCG(s.seed2(i+1), 0x737562))}, sub: true}
	w.emit(rec{"type": "started", "agentId": s.id + ":a" + fmt.Sprint(i+1), "key": "task",
		"prompt": w.sentence(4, 12)})
	for int64(len(w.buf)) < c.MainMeanBytes/4 {
		w.turn()
	}
	w.emit(rec{"type": "result", "agentId": s.id + ":a" + fmt.Sprint(i+1),
		"result": w.sentence(4, 16)})
	return w.buf
}

func (s *sessionGen) seed2(i int) uint64 {
	return uint64(s.idx)<<16 | uint64(i)<<8 | uint64(s.seed)&0xff
}

// turn emits one conversation turn: a user prompt, optionally an attachment,
// one assistant response of one to four blocks sharing a message id and
// usage, one tool_result record per tool_use block, and sometimes a system
// record. Block and result sizes drive the corpus's size distribution.
func (w *sessionWriter) turn() {
	msgBase := rec{"sessionId": w.s.id, "cwd": w.s.cwd, "version": "2.1.280",
		"gitBranch": w.s.branch, "userType": "external"}
	if w.sub {
		msgBase["isSidechain"] = true
		msgBase["agentId"] = w.s.id + ":a1"
	}

	w.emit(with(msgBase, rec{"type": "user", "message": rec{
		"role": "user", "content": w.text(60, 1800)}}))

	if w.rng.Float64() < 0.35 {
		w.emit(with(msgBase, rec{"type": "attachment", "attachment": rec{
			"type": "environment", "env": w.sentence(6, 20)}}))
	}

	msgID := fmt.Sprintf("msg_%06d", w.uuid)
	usage := rec{
		"input_tokens":                400 + w.rng.IntN(50_000),
		"cache_read_input_tokens":     2000 + w.rng.IntN(180_000),
		"cache_creation_input_tokens": w.rng.IntN(8000),
		"output_tokens":               100 + w.rng.IntN(4000),
		"service_tier":                "standard",
	}
	blocks := 0
	if w.rng.Float64() < 0.5 {
		w.emit(responseBlock(msgBase, msgID, usage, 0, rec{"type": "thinking",
			"thinking": w.text(300, 900), "signature": w.words(6)}))
		blocks++
	}
	w.emit(responseBlock(msgBase, msgID, usage, blocks, rec{"type": "text",
		"text": w.text(200, 2500)}))
	blocks++
	for t := 0; t < w.rng.IntN(3); t++ {
		name := tools[w.rng.IntN(len(tools))]
		w.emit(responseBlock(msgBase, msgID, usage, blocks, rec{"type": "tool_use",
			"name": name, "input": toolInput(w, name)}))
		blocks++
		w.emit(with(msgBase, rec{"type": "user",
			"sourceToolAssistantUUID": w.uuidString(w.uuid),
			"message": rec{"role": "user", "content": []any{rec{
				"type": "tool_result", "tool_use_id": w.uuidString(w.uuid),
				"content": w.text(300, 24_000), "is_error": w.rng.Float64() < 0.05,
			}}},
			"toolUseResult": rec{"stdout": w.text(100, 800), "exitCode": 0}}))
	}
	if w.rng.Float64() < 0.3 {
		w.emit(with(msgBase, rec{"type": "system", "subtype": "turn_duration",
			"level": "info", "durationMs": w.rng.IntN(120_000)}))
	}
}

func responseBlock(base rec, msgID string, usage rec, index int, block rec) rec {
	return with(base, rec{"type": "assistant", "apiBlockIndex": index,
		"message": rec{"role": "assistant", "id": msgID,
			"model": "claude-opus-4-6", "content": []any{block}, "usage": usage}})
}

func with(base, extra rec) rec {
	out := rec{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// Text generation with a skewed word distribution: common words dominate,
// markers stay rare, so FTS queries see both wide and selective terms.
// textGen is embedded by every agent's writer, so all corpora share one
// distribution and per-agent benchmarks compare envelopes, not text volume.

type textGen struct{ rng *rand.Rand }

var vocabulary = []string{
	"function", "parser", "session", "cursor", "batch", "record", "offset",
	"query", "index", "schema", "migration", "checkpoint", "watcher", "poll",
	"commit", "retry", "device", "source", "event", "aggregate", "token",
	"annotation", "writer", "reader", "connection", "deadline", "limit",
	"wal", "sqlite", "trigger", "table", "column",
	"write", "read", "test", "error", "value", "field", "string", "bytes",
	"line", "file", "path", "identity", "registry", "invitation",
	"credential", "endpoint", "frame", "protocol", "uploader",
	"transport", "backoff", "timeout", "context", "cancel",
	"go", "run", "build", "check", "bench", "profile", "cpu", "memory",
	"latency", "throughput", "regression", "fixture", "corpus", "volume",
	"report", "measure", "document", "operator", "recovery", "backup",
	"upgrade", "install", "binary", "flag", "config", "state", "help",
}

// markers appear in a small fraction of texts; each is a selective
// full-text search term.
var markers = []string{
	"authentication", "refresh", "bearer", "deadlock", "fingerprint",
	"synthetic", "notarization", "quarantine",
}

var tools = []string{"Bash", "Read", "Write", "Edit", "Grep", "Glob", "Task"}

var projects = []string{
	"ohya", "ohya-docs", "server", "cli-tool", "parser-lab", "webapp",
	"experiments", "notes",
}

var branches = []string{"main", "perf/benchmarks", "fix-sync-cursor", "release-1.0"}

func (t textGen) words(n int) string {
	out := make([]byte, 0, n*10)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, t.word()...)
	}
	return string(out)
}

func (t textGen) word() string {
	// Squared index: the first words dominate, the tail stays rare.
	f := t.rng.Float64()
	i := int(f * f * float64(len(vocabulary)))
	return vocabulary[i]
}

func (t textGen) sentence(lo, hi int) string {
	n := lo + t.rng.IntN(hi-lo+1)
	return t.words(n) + "."
}

// text emits text of approximately min to max bytes. About three percent of
// texts carry one marker word, so each marker matches a small, stable share
// of events; a third of the filler lines look like code.
func (t textGen) text(min, max int) string {
	n := min + t.rng.IntN(max-min+1)
	var out []byte
	if t.rng.Float64() < 0.03 {
		out = append(out, markers[t.rng.IntN(len(markers))]...)
		out = append(out, ' ')
	}
	for len(out) < n {
		switch {
		case t.rng.Float64() < 0.3:
			out = append(out, t.codeLine()...)
			out = append(out, '\n')
		default:
			out = append(out, t.words(3+t.rng.IntN(9))...)
			out = append(out, ' ')
		}
	}
	return string(out[:n])
}

func (t textGen) codeLine() string {
	switch t.rng.IntN(3) {
	case 0:
		return fmt.Sprintf("\t%s := %s.%s(%d)", t.word(), t.word(), t.word(), t.rng.IntN(9999))
	case 1:
		return fmt.Sprintf("SELECT %s FROM %s WHERE %s = %d", t.word(), t.word(), t.word(), t.rng.IntN(9999))
	default:
		return fmt.Sprintf("%s/%s.jsonl:%d: %s", t.word(), t.word(), t.rng.IntN(9999), t.words(4))
	}
}

func toolInput(w *sessionWriter, name string) rec {
	switch name {
	case "Bash":
		return rec{"command": "go test -run " + w.words(1) + " ./internal/" + w.word(),
			"description": w.sentence(3, 10)}
	case "Read", "Write", "Edit":
		return rec{"file_path": "/home/agent/" + w.word() + "/" + w.word() + ".go",
			"content": w.text(100, 2000)}
	default:
		return rec{"pattern": w.word() + "*", "path": "/home/agent/" + w.word(),
			"query": w.words(3)}
	}
}

// epochMS is 2026-01-01T00:00:00.000Z in Unix milliseconds.
const epochMS int64 = 1767225600_000

// isoMS renders Unix milliseconds as the ISO 8601 UTC form Claude Code
// writes, from a fixed epoch so output stays deterministic.
func isoMS(ms int64) string {
	sec, msec := (epochMS+ms)/1000, (epochMS+ms)%1000
	days, rem := sec/86400, sec%86400
	h, mi, s := rem/3600, rem%3600/60, rem%60
	y, mo, d := civilFromDays(days)
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%03dZ", y, mo, d, h, mi, s, msec)
}

// civilFromDays converts days since 1970-01-01 to a civil date (Howard
// Hinnant's algorithm; valid for the whole range a corpus can reach).
func civilFromDays(z int64) (int64, int64, int64) {
	z += 719468
	era := z / 146097
	doe := z - era*146097 // [0, 146096]
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
		y++
	}
	return y, m, d
}
