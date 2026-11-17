package hubdata

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	_ "github.com/mattn/go-sqlite3"
)

func ReadOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	u.RawQuery = url.Values{
		"mode":          []string{"ro"},
		"_busy_timeout": []string{"5000"},
	}.Encode()
	return u.String()
}

func OpenReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", ReadOnlyDSN(abs))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

type Session struct {
	ID         string
	DeviceID   string
	Agent      string
	EventCount int64
}

func LargestSessions(db *sql.DB, limit int) ([]Session, error) {
	rows, err := db.Query(`
		SELECT id, device_id, agent, event_count
		FROM sessions
		WHERE event_count > 0
		ORDER BY event_count DESC, id
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.DeviceID, &s.Agent, &s.EventCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func AllSessions(db *sql.DB) ([]Session, error) {
	rows, err := db.Query(`
		SELECT id, device_id, agent, event_count
		FROM sessions
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.DeviceID, &s.Agent, &s.EventCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func ReadSessionRaw(db *sql.DB, sessionID string, maxBytes int64) ([][]byte, int64, error) {
	rows, err := db.Query(`
		SELECT raw_json FROM events
		WHERE session_id = ?
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out [][]byte
	var total int64
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, 0, err
		}
		if maxBytes > 0 && total+int64(len(raw)) > maxBytes {
			return out, total, nil
		}
		out = append(out, []byte(raw))
		total += int64(len(raw))
	}
	return out, total, rows.Err()
}

func BuildBlocks(records [][]byte, target int) [][]byte {
	if target <= 0 {
		return nil
	}
	var blocks [][]byte
	var cur []byte
	for _, rec := range records {
		cur = append(cur, rec...)
		cur = append(cur, '\n')
		if len(cur) >= target {
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks
}

type DupTotals struct {
	RawBytes    int64
	StringBytes int64
	RepeatBytes int64
}

func (d DupTotals) LargeStringShare() float64 {
	if d.RawBytes == 0 {
		return 0
	}
	return float64(d.StringBytes) / float64(d.RawBytes)
}

func (d DupTotals) RepeatShare() float64 {
	if d.RawBytes == 0 {
		return 0
	}
	return float64(d.RepeatBytes) / float64(d.RawBytes)
}

func HashKey(s string) [16]byte {
	sum := sha256.Sum256([]byte(s))
	var key [16]byte
	copy(key[:], sum[:16])
	return key
}

func WalkLargeStrings(raw []byte, minLen int, seen map[[16]byte]struct{}, totals *DupTotals) {
	totals.RawBytes += int64(len(raw))
	VisitLargeStrings(raw, minLen, func(s string) {
		totals.StringBytes += int64(len(s))
		key := HashKey(s)
		if _, dup := seen[key]; dup {
			totals.RepeatBytes += int64(len(s))
		} else {
			seen[key] = struct{}{}
		}
	})
}

func VisitLargeStrings(raw []byte, minLen int, visit func(s string)) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		s, ok := tok.(string)
		if !ok || len(s) < minLen {
			continue
		}
		visit(s)
	}
}

func ToolOutput(agent, kind, role, toolName, payloadType, metadata string) bool {
	switch agent {
	case "pi":
		return role == "toolResult"
	case "codex":
		return kind == "response_item" && role == "" && toolName == "" &&
			(payloadType == "function_call_output" || payloadType == "custom_tool_call_output")
	case "claude-code":
		if kind != "user" || role != "user" {
			return false
		}
		return metadataHasToolResult(metadata)
	}
	return false
}

func metadataHasToolResult(metadata string) bool {
	var md map[string]any
	if err := json.Unmarshal([]byte(metadata), &md); err != nil {
		return false
	}
	if _, ok := md["toolUseResult"]; ok {
		return true
	}
	blocks, ok := md["blocks"].([]any)
	if !ok {
		return false
	}
	for _, item := range blocks {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := b["type"].(string); t == "tool_result" || t == "mcp_tool_result" {
			return true
		}
	}
	return false
}

func TrainDictionary(dir string, samples [][]byte, maxSize int) ([]byte, error) {
	zstd, err := exec.LookPath("zstd")
	if err != nil {
		return nil, fmt.Errorf("zstd CLI not found: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var paths []string
	for i, s := range samples {
		p := filepath.Join(dir, "s"+strconv.Itoa(i)+".bin")
		if err := os.WriteFile(p, s, 0o600); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	defer func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}()
	out := filepath.Join(dir, "dictionary.bin")
	args := append([]string{"--train", "-q"}, paths...)
	args = append(args, "-o", out, "--maxdict="+strconv.Itoa(maxSize))
	cmd := exec.Command(zstd, args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("zstd --train: %v: %s", err, string(combined))
	}
	return os.ReadFile(out)
}

func DictSamples(records [][]byte, maxCount, maxSampleBytes int) [][]byte {
	var candidates [][]byte
	for _, r := range records {
		if len(r) <= maxSampleBytes {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) <= maxCount {
		return candidates
	}
	step := float64(len(candidates)) / float64(maxCount)
	out := make([][]byte, 0, maxCount)
	for i := 0; i < maxCount; i++ {
		out = append(out, candidates[int(float64(i)*step)])
	}
	return out
}

func SplitHalves(records [][]byte) ([][]byte, [][]byte) {
	var total int
	for _, r := range records {
		total += len(r)
	}
	half := total / 2
	var acc int
	for i, r := range records {
		acc += len(r)
		if acc >= half {
			rest := make([][]byte, len(records)-i-1)
			copy(rest, records[i+1:])
			return records[: i+1 : i+1], rest
		}
	}
	return records, nil
}
