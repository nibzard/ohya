package fixgen

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
)

// GeneratePi returns every file of a deterministic synthetic Pi agent
// session corpus (testdata/pi/README.md, format v3): one append-only JSONL
// file per session under its encoded-cwd directory, in the 1.5.2 shape (the
// system preamble entry and durationMs fields; the parser tolerates their
// absence the same way). Pi sessions never span files and have no subagent
// transcripts, so Corpus.SubagentsPerSession has no Pi equivalent and stays
// unused.
func GeneratePi(c Corpus) []File {
	rng := rand.New(rand.NewPCG(uint64(c.seed()), 0x00706921))
	var files []File
	for i := 0; i < c.Sessions; i++ {
		s := newPiSession(c, rng, i)
		files = append(files, File{Path: s.path(), Data: s.json(c)})
	}
	return files
}

// WritePi writes the Pi corpus under root, mirroring the sessions tree
// layout (--encoded-cwd--/<timestamp>_<session-uuid>.jsonl), and returns the
// total bytes written.
func WritePi(root string, c Corpus) (int64, error) {
	return writeFiles(root, GeneratePi(c))
}

// piGen carries one session's deterministic state.
type piGen struct {
	idx    int
	cwd    string
	id     string // session UUID; also the file name's UUID
	seed   int64
	baseMS int64
}

func newPiSession(c Corpus, rng *rand.Rand, idx int) *piGen {
	// Pi has no subagent files; the corpus rng only spreads session starts.
	return &piGen{
		idx:    idx,
		cwd:    "/home/agent/" + projects[idx%len(projects)],
		id:     uuid(0x01a10000+uint64(idx), uint64(idx)),
		seed:   c.seed(),
		baseMS: int64(idx)*86_400_000/3 + int64(rng.IntN(3_600_000)),
	}
}

// encodePiCwd applies Pi's lossy session-directory encoding: the cwd minus
// one leading slash, every '/', '\' and ':' replaced by '-', wrapped in
// double dashes (testdata/pi/README.md). Never decoded.
func encodePiCwd(cwd string) string {
	enc := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return "--" + enc.Replace(strings.TrimPrefix(cwd, "/")) + "--"
}

func (g *piGen) path() string {
	name := strings.NewReplacer(":", "-", ".", "-").Replace(isoMS(g.baseMS))
	return filepath.Join(encodePiCwd(g.cwd), name+"_"+g.id+".jsonl")
}

// piWriter emits one session file's entries as JSONL bytes. Entries form a
// chain: each names the previous entry as its parent, the first after the
// header parented at null.
type piWriter struct {
	textGen
	g    *piGen
	buf  []byte
	seq  int64 // entry counter; ids are 8-hex like the real format
	ms   int64
	call int64
	resp int64
}

// raw appends one record without the entry framing — for the session header,
// which carries no entry id or parent.
func (w *piWriter) raw(r rec) {
	b, err := json.Marshal(r)
	if err != nil {
		panic("fixgen: marshal pi record: " + err.Error())
	}
	w.buf = append(w.buf, b...)
	w.buf = append(w.buf, '\n')
}

func (w *piWriter) emit(r rec) {
	w.seq++
	parent := any(nil)
	if w.seq > 1 {
		parent = fmt.Sprintf("%08x", w.seq-1)
	}
	r["id"] = fmt.Sprintf("%08x", w.seq)
	r["parentId"] = parent
	w.ms += int64(20 + w.rng.IntN(4000))
	r["timestamp"] = isoMS(w.g.baseMS + w.ms)
	w.raw(r)
}

// json generates one session file: the header, model and thinking-level
// changes, the system preamble entry, then turns until the target size.
func (g *piGen) json(c Corpus) []byte {
	w := &piWriter{textGen: textGen{rng: rand.New(rand.NewPCG(g.seed3(), 0x70697365))}, g: g}
	w.raw(rec{"type": "session", "version": 3, "id": g.id,
		"timestamp": isoMS(g.baseMS), "cwd": g.cwd})
	w.emit(rec{"type": "model_change", "provider": "openai", "modelId": "gpt-5.4-nano"})
	w.emit(rec{"type": "thinking_level_change", "thinkingLevel": "off"})
	w.emit(rec{"type": "message", "message": rec{
		"role": "system", "content": "",
		"sections": rec{
			"preamble": "You are a coding assistant running inside pi.",
			"tools":    "<tools>\n- read: Read file contents\n- bash: Execute bash commands\n</tools>",
		},
		"timestamp":  epochMS + w.ms,
		"toolsAdded": piToolsAdded,
	}})
	for int64(len(w.buf)) < c.MainMeanBytes {
		w.turn()
	}
	return w.buf
}

func (g *piGen) seed3() uint64 {
	return uint64(g.idx)<<16 | uint64(g.seed)&0xff
}

// turn emits one conversation turn: the user prompt, one to three tool
// round-trips (assistant toolCall response plus its toolResult), and the
// final assistant text response. Every assistant message carries exactly one
// per-response usage object keyed by its responseId.
func (w *piWriter) turn() {
	w.emit(rec{"type": "message", "message": rec{
		"role":      "user",
		"content":   []any{rec{"type": "text", "text": w.text(60, 1800)}},
		"timestamp": epochMS + w.ms}})

	for t := 0; t <= w.rng.IntN(3); t++ {
		w.call++
		w.resp++
		name := piTools[w.rng.IntN(len(piTools))]
		callID := fmt.Sprintf("call_%08x_%06d|fc_%024x", uint64(w.g.idx), w.seq, uint64(w.call))
		w.emit(rec{"type": "message", "message": rec{
			"role": "assistant",
			"content": []any{rec{"type": "toolCall", "id": callID, "name": name,
				"arguments": w.toolArguments(name)}},
			"api": "openai-responses", "provider": "openai", "model": "gpt-5.4-nano",
			"usage":      w.usage(),
			"stopReason": "toolUse", "rawStopReason": "completed",
			"timestamp": epochMS + w.ms, "responseId": w.responseID(),
			"durationMs": 500 + w.rng.IntN(10_000), "thinkingLevel": "off"}})
		isError := w.rng.Float64() < 0.05
		w.emit(rec{"type": "message", "message": rec{
			"role": "toolResult", "toolCallId": callID, "toolName": name,
			"content": []any{rec{"type": "text", "text": w.text(300, 24_000)}},
			"isError": isError, "durationMs": w.rng.IntN(5000),
			"timestamp": epochMS + w.ms}})
	}

	w.resp++
	w.emit(rec{"type": "message", "message": rec{
		"role": "assistant",
		"content": []any{rec{"type": "text", "text": w.text(200, 2500),
			"textSignature": compact(rec{"v": 1, "id": w.id("msg"), "phase": "final_answer"})}},
		"api": "openai-responses", "provider": "openai", "model": "gpt-5.4-nano",
		"usage":      w.usage(),
		"stopReason": "stop", "rawStopReason": "completed",
		"timestamp": epochMS + w.ms, "responseId": w.responseID(),
		"durationMs": 500 + w.rng.IntN(10_000), "thinkingLevel": "off"}})
}

func (w *piWriter) id(kind string) string {
	return fmt.Sprintf("%s_%s_%06d", kind, w.g.id[:8], w.seq)
}

func (w *piWriter) responseID() string {
	return fmt.Sprintf("resp_%s_%06d", w.g.id[:8], w.resp)
}

// usage draws one per-response usage object. The verified identity
// input + cacheRead + output = totalTokens holds by construction, and
// cacheWrite and reasoning stay at the verified zero.
func (w *piWriter) usage() rec {
	in := 400 + w.rng.IntN(50_000)
	cacheRead := 2000 + w.rng.IntN(180_000)
	out := 100 + w.rng.IntN(4000)
	total := in + cacheRead + out
	return rec{
		"input": in, "output": out, "cacheRead": cacheRead,
		"cacheWrite": 0, "reasoning": 0, "totalTokens": total,
		"cost": rec{
			"input": float64(in) * 2e-7, "output": float64(out) * 3e-7,
			"cacheRead": float64(cacheRead) * 2e-8, "cacheWrite": 0,
			"total": float64(in)*2e-7 + float64(out)*3e-7 + float64(cacheRead)*2e-8,
		},
	}
}

// toolArguments builds one tool call's arguments object.
func (w *piWriter) toolArguments(name string) rec {
	switch name {
	case "bash":
		return rec{"command": w.sentence(2, 6), "timeout": 10 + w.rng.IntN(50)}
	case "read", "edit":
		return rec{"path": "/home/agent/" + w.word() + "/" + w.word() + ".go"}
	default:
		return rec{"pattern": w.word() + "*", "path": "/home/agent/" + w.word()}
	}
}

var piTools = []string{"read", "bash", "grep", "edit", "list"}

var piToolsAdded = []any{
	rec{"name": "read", "description": "Read the contents of a file."},
	rec{"name": "bash", "description": "Execute a bash command."},
	rec{"name": "grep", "description": "Search file contents with a pattern."},
}
