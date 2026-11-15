package fixgen

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

type Corpus struct {
	Sessions            int
	MainMeanBytes       int64
	SubagentsPerSession float64
	Seed                int64
	Compact             bool
}

type File struct {
	Path string
	Data []byte
}

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

func Write(root string, c Corpus) (int64, error) {
	return writeFiles(root, Generate(c))
}

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

func uuid(hi, n uint64) string {
	return fmt.Sprintf("%08x-0000-7000-8000-%012x", hi, n)
}

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

type sessionGen struct {
	idx       int
	project   string
	cwd       string
	branch    string
	id        string
	seed      int64
	subagents int
	baseMS    int64
}

func newSession(c Corpus, rng *rand.Rand, idx int) *sessionGen {
	proj := projects[idx%len(projects)]
	s := &sessionGen{
		idx:    idx,
		cwd:    "/home/agent/" + proj,
		branch: branches[idx%len(branches)],
		id:     fmt.Sprintf("%08x-0000-4000-8000-%012x", 0x5e550000+uint64(idx), uint64(idx)),
		seed:   c.seed(),
		baseMS: int64(idx) * 86_400_000 / 3,
	}
	s.project = encodeCwd(s.cwd)
	if c.SubagentsPerSession > 0 {
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

type rec map[string]any

type sessionWriter struct {
	s *sessionGen
	textGen
	subagent bool
	buf      []byte
	uuid     int64
	ms       int64
}

func (w *sessionWriter) emit(r rec) {
	w.uuid++
	r["uuid"] = w.uuidString(w.uuid)
	r["parentUuid"] = w.uuidString(w.uuid - 1)
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

func (s *sessionGen) mainJSON(c Corpus) []byte {
	w := &sessionWriter{s: s, textGen: newTextGen(s.seed2(0), 0x6d61696e, c)}
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

func (s *sessionGen) subJSON(c Corpus, i int) []byte {
	w := &sessionWriter{s: s, textGen: newTextGen(s.seed2(i+1), 0x737562, c), subagent: true}
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

func (w *sessionWriter) turn() {
	msgBase := rec{"sessionId": w.s.id, "cwd": w.s.cwd, "version": "2.1.280",
		"gitBranch": w.s.branch, "userType": "external"}
	if w.subagent {
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

type textGen struct {
	rng *rand.Rand
	div int
}

func newTextGen(x, y uint64, c Corpus) textGen {
	t := textGen{rng: rand.New(rand.NewPCG(x, y))}
	if c.Compact {
		t.div = compactTextDiv
	}
	return t
}

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
	f := t.rng.Float64()
	i := int(f * f * float64(len(vocabulary)))
	return vocabulary[i]
}

func (t textGen) sentence(lo, hi int) string {
	n := lo + t.rng.IntN(hi-lo+1)
	return t.words(n) + "."
}

func (t textGen) text(min, max int) string {
	if t.div > 1 {
		lo, hi := min/t.div, max/t.div
		if lo < 8 {
			lo = 8
		}
		if hi < lo {
			hi = lo
		}
		min, max = lo, hi
	}
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

const epochMS int64 = 1767225600_000

func isoMS(ms int64) string {
	return time.UnixMilli(epochMS + ms).UTC().Format("2006-01-02T15:04:05.000Z")
}
