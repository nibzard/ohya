package fixgen

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
)

// GenerateCodex returns every file of a deterministic synthetic Codex rollout
// corpus (testdata/codex/README.md): one main rollout per session under
// date directories, plus subagent rollouts that open with their own
// session_meta and embed the parent's as the second line. One thread spans
// one file here; resumed pages exercise write-path dedup, not parse cost,
// and stay out of the corpus.
func GenerateCodex(c Corpus) []File {
	rng := rand.New(rand.NewPCG(uint64(c.seed()), 0x636f646578))
	var files []File
	for i := 0; i < c.Sessions; i++ {
		s := newCodexThread(c, rng, i)
		files = append(files, File{Path: s.mainPath(), Data: s.mainJSON(c)})
		for sub := 0; sub < s.subagents; sub++ {
			files = append(files, File{Path: s.subPath(sub), Data: s.subJSON(c, sub)})
		}
	}
	return files
}

// WriteCodex writes the Codex corpus under root, mirroring the sessions tree
// layout (YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl), and returns the
// total bytes written.
func WriteCodex(root string, c Corpus) (int64, error) {
	return writeFiles(root, GenerateCodex(c))
}

// codexGen carries the per-thread deterministic state shared by a main
// rollout and its subagent rollouts.
type codexGen struct {
	idx       int
	cwd       string
	branch    string
	thread    string // thread UUID; also the main rollout's name UUID
	seed      int64
	subagents int
	baseMS    int64
}

func newCodexThread(c Corpus, rng *rand.Rand, idx int) *codexGen {
	g := &codexGen{
		idx:    idx,
		cwd:    "/home/agent/" + projects[idx%len(projects)],
		branch: branches[idx%len(branches)],
		thread: uuid(0x07e1c000, uint64(idx)),
		seed:   c.seed(),
		baseMS: int64(idx) * 86_400_000 / 3,
	}
	if c.SubagentsPerSession > 0 {
		g.subagents = int(rng.Float64() * 2 * c.SubagentsPerSession)
		if g.subagents > 4 {
			g.subagents = 4
		}
	}
	return g
}

func (g *codexGen) subagentID(i int) string {
	return uuid(0x07e1d000, uint64(g.idx)*16+uint64(i))
}

func (g *codexGen) seed2(i int) uint64 {
	return uint64(g.idx)<<16 | uint64(i)<<8 | uint64(g.seed)&0xff
}

// rolloutTimestamp renders the file-name timestamp form observed in the real
// sessions tree: seconds precision, colons replaced by dashes.
func rolloutTimestamp(ms int64) string {
	ts := isoMS(ms)
	return strings.ReplaceAll(ts[:len(ts)-5], ":", "-") // drop ".mmmZ"
}

func (g *codexGen) mainPath() string {
	return filepath.Join(dateDirs(g.baseMS),
		fmt.Sprintf("rollout-%s-%s.jsonl", rolloutTimestamp(g.baseMS), g.thread))
}

func (g *codexGen) subPath(i int) string {
	id := g.subagentID(i)
	return filepath.Join(dateDirs(g.baseMS),
		fmt.Sprintf("rollout-%s-%s.jsonl",
			rolloutTimestamp(g.baseMS+int64(i+1)*61_000), id))
}

// dateDirs returns the YYYY/MM/DD path of a session start, the way Codex
// files sessions by local start time (corpus times are UTC).
func dateDirs(ms int64) string {
	ts := isoMS(ms)
	return filepath.Join(ts[0:4], ts[5:7], ts[8:10])
}

// codexWriter emits one rollout file's records as JSONL bytes. Ordinals grow
// by one per record within the thread, starting at the session_meta record.
type codexWriter struct {
	textGen
	g   *codexGen
	sub string // this rollout's own thread id (a subagent's, or the main's)
	buf []byte
	ord int64
	ms  int64
	seq int64
	// Running token totals the usage records carry beside their per-response
	// usage: turn totals reset each turn, thread totals accumulate.
	turnIn, turnCache, turnOut       int64
	threadIn, threadCache, threadOut int64
}

func (w *codexWriter) emit(r rec) {
	r["ordinal"] = w.ord
	w.ord++
	w.ms += int64(20 + w.rng.IntN(4000))
	r["timestamp"] = isoMS(w.g.baseMS + w.ms)
	w.seq++
	b, err := json.Marshal(r)
	if err != nil {
		panic("fixgen: marshal codex record: " + err.Error())
	}
	w.buf = append(w.buf, b...)
	w.buf = append(w.buf, '\n')
}

func (w *codexWriter) id(kind string) string {
	return fmt.Sprintf("%s_%s_%06d", kind, w.g.thread[:8], w.seq)
}

// sessionMeta builds one session_meta record. id is the thread the meta
// names; parent is set for a subagent's own meta (source.subagent pointing
// at the parent thread), and tsMS names the moment the meta describes.
func (w *codexWriter) sessionMeta(id, parent string, tsMS int64) rec {
	payload := rec{
		"id": id, "timestamp": isoMS(tsMS),
		"cwd": w.g.cwd, "originator": "codex-tui", "cli_version": "0.166.0",
		"source": "cli", "model_provider": "openai", "history_mode": "legacy",
		"git": rec{
			"commit_hash":    fmt.Sprintf("%040x", uint64(w.seq)^uint64(w.g.idx)),
			"branch":         w.g.branch,
			"repository_url": "https://example.com/agent/" + projects[w.g.idx%len(projects)] + ".git",
		},
	}
	if parent != "" {
		payload["source"] = rec{"subagent": rec{"thread_spawn": rec{
			"parent_thread_id": parent, "depth": 1, "agent_path": "/root/search",
			"agent_nickname": nicknames[int(w.g.idx)%len(nicknames)], "agent_role": nil,
		}}}
		payload["parent_thread_id"] = parent
		payload["forked_from_id"] = parent
		delete(payload, "git")
		delete(payload, "history_mode")
	}
	return rec{"type": "session_meta", "payload": payload}
}

// mainJSON generates one main rollout: the thread's session_meta, a
// developer permissions message, then turns until the target size.
func (g *codexGen) mainJSON(c Corpus) []byte {
	w := &codexWriter{
		textGen: textGen{rng: rand.New(rand.NewPCG(g.seed2(0), 0x726f6c6c))},
		g:       g, sub: g.thread,
	}
	w.emit(w.sessionMeta(g.thread, "", g.baseMS))
	w.emit(rec{"type": "response_item", "payload": rec{
		"type": "message", "id": w.id("msg"), "role": "developer",
		"content": []any{rec{"type": "input_text",
			"text": "<permissions instructions>\nSandbox is read-only. Approval policy is never.\n</permissions instructions>"}}}})
	for int64(len(w.buf)) < c.MainMeanBytes {
		w.turn()
	}
	return w.buf
}

// subJSON generates one subagent rollout of about a quarter of the main
// file's size: its own meta naming the parent thread, the parent's meta
// embedded as line two, then turns until the target size.
func (g *codexGen) subJSON(c Corpus, i int) []byte {
	id := g.subagentID(i)
	w := &codexWriter{
		textGen: textGen{rng: rand.New(rand.NewPCG(g.seed2(i+1), 0x73756263))},
		g:       g, sub: id,
	}
	w.emit(w.sessionMeta(id, g.thread, g.baseMS+int64(i+1)*60_000))
	w.emit(w.sessionMeta(g.thread, "", g.baseMS))
	for int64(len(w.buf)) < c.MainMeanBytes/4 {
		w.turn()
	}
	return w.buf
}

// turn emits one Codex conversation turn: the user prompt mirrored by a
// user_message event, a turn context, one to three shell tool calls with
// their outputs and exec_command_end events, the assistant answer mirrored
// by an agent_message event, one token_usage_record per API response with
// running turn and thread totals, and task_complete.
func (w *codexWriter) turn() {
	w.turnIn, w.turnCache, w.turnOut = 0, 0, 0
	turnID := uuid(0x7475726e, uint64(w.g.idx)<<20|uint64(w.seq))

	prompt := w.text(60, 1800)
	w.emit(rec{"type": "response_item", "payload": rec{
		"type": "message", "id": w.id("msg"), "role": "user",
		"content": []any{rec{"type": "input_text", "text": prompt}}}})
	w.emit(rec{"type": "event_msg", "payload": rec{
		"type": "user_message", "message": prompt,
		"images": []any{}, "local_images": []any{}, "text_elements": []any{}}})
	w.emit(rec{"type": "turn_context", "payload": rec{
		"turn_id": turnID, "root_turn_id": turnID, "cwd": w.g.cwd,
		"approval_policy": "never", "sandbox_policy": rec{"type": "read-only"},
		"model": "gpt-5-codex", "effort": "medium", "summary": "none"}})

	for t := 0; t <= w.rng.IntN(3); t++ {
		if w.rng.Float64() < 0.5 {
			w.emit(rec{"type": "response_item", "payload": rec{
				"type": "reasoning", "id": w.id("rs"),
				"summary":           []any{rec{"type": "summary_text", "text": w.text(300, 900)}},
				"content":           nil,
				"encrypted_content": fmt.Sprintf("gAAAAABfixgen-%08x-%06d", uint64(w.g.idx), w.seq)}})
		}
		call := w.id("call")
		argv, output := w.shellCall()
		exit := 0
		if w.rng.Float64() < 0.05 {
			exit = 1
		}
		w.emit(rec{"type": "response_item", "payload": rec{
			"type": "function_call", "id": w.id("fc"), "status": "completed",
			"call_id": call, "name": "shell",
			"arguments": compact(rec{"command": argv})}})
		w.emit(rec{"type": "response_item", "payload": rec{
			"type": "function_call_output", "id": w.id("fco"),
			"call_id": call, "output": output}})
		w.emit(rec{"type": "event_msg", "payload": rec{
			"type": "exec_command_end", "call_id": call, "command": argv,
			"cwd": w.g.cwd, "stdout": "", "stderr": "",
			"aggregated_output": output, "exit_code": exit, "status": "completed"}})
		w.usageRecord(turnID)
	}

	answer := w.text(200, 2500)
	w.emit(rec{"type": "response_item", "payload": rec{
		"type": "message", "status": "completed", "id": w.id("msg"),
		"role": "assistant", "phase": "final",
		"content": []any{rec{"type": "output_text", "text": answer,
			"annotations": []any{}, "logprobs": []any{}}}}})
	w.emit(rec{"type": "event_msg", "payload": rec{
		"type": "agent_message", "message": answer, "phase": "final",
		"memory_citation": nil}})
	w.usageRecord(turnID)
	w.emit(rec{"type": "event_msg", "payload": rec{
		"type": "task_complete", "turn_id": turnID, "last_agent_message": answer}})
}

// usageRecord emits one token_usage_record for the response that just ended:
// per-response usage plus the running turn and thread totals the real format
// carries on the same record.
func (w *codexWriter) usageRecord(turnID string) {
	in := 2000 + w.rng.IntN(180_000)
	cached := 1000 + w.rng.IntN(150_000)
	if cached > in {
		// Every verified record keeps the cached part within input_tokens.
		in, cached = cached, in
	}
	out := 100 + w.rng.IntN(4000)
	usage := func(in, cached, out int64) rec {
		return rec{
			"input_tokens": in, "cached_input_tokens": cached,
			"cache_write_input_tokens": 0, "output_tokens": out,
			"reasoning_output_tokens": w.rng.IntN(2000),
			"total_tokens":            in + out,
		}
	}
	w.turnIn += int64(in)
	w.turnCache += int64(cached)
	w.turnOut += int64(out)
	w.threadIn += int64(in)
	w.threadCache += int64(cached)
	w.threadOut += int64(out)
	w.emit(rec{"type": "token_usage_record", "payload": rec{
		"thread_id": w.sub, "session_id": w.g.thread, "turn_id": turnID,
		"response_id":        w.id("resp"),
		"usage":              usage(int64(in), int64(cached), int64(out)),
		"turn_token_usage":   usage(w.turnIn, w.turnCache, w.turnOut),
		"thread_token_usage": usage(w.threadIn, w.threadCache, w.threadOut),
	}})
}

// shellCall builds the argument vector of one shell call and the output its
// command prints, mirroring the commands a coding agent runs.
func (w *codexWriter) shellCall() (argv []any, output string) {
	switch w.rng.IntN(3) {
	case 0:
		argv = []any{"go", "test", "./internal/" + w.word()}
	case 1:
		argv = []any{"sed", "-n", "1,50p", w.word() + ".md"}
	default:
		argv = []any{"rg", "-n", w.words(2), w.word()}
	}
	return argv, w.text(300, 24_000)
}

var nicknames = []string{"Finch", "Robin", "Wren", "Sparrow"}
