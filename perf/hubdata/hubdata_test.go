package hubdata

import (
	"database/sql"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestBuildBlocksSplitsAtRecordBoundaries(t *testing.T) {
	rec := []byte(strings.Repeat("a", 100))
	records := [][]byte{rec, rec, rec}
	blocks := BuildBlocks(records, 150)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if len(blocks[0]) != 202 {
		t.Fatalf("first block = %d bytes, want 202", len(blocks[0]))
	}
	var total int
	for _, b := range blocks {
		if !strings.HasSuffix(string(b), "\n") {
			t.Fatalf("block does not end with newline")
		}
		total += len(b)
	}
	if total != 303 {
		t.Fatalf("total = %d, want 303", total)
	}
}

func TestBuildBlocksKeepsShortTail(t *testing.T) {
	records := [][]byte{[]byte("x")}
	blocks := BuildBlocks(records, 4096)
	if len(blocks) != 1 || string(blocks[0]) != "x\n" {
		t.Fatalf("blocks = %v", blocks)
	}
}

func large(n int) string {
	return strings.Repeat("s", n)
}

func TestWalkLargeStringsCountsRepeats(t *testing.T) {
	shared := large(300)
	raw1 := []byte(`{"a":"` + shared + `","b":"` + large(100) + `"}`)
	raw2 := []byte(`{"a":"` + shared + `","c":"` + large(400) + `"}`)
	seen := map[[16]byte]struct{}{}
	var totals DupTotals
	WalkLargeStrings(raw1, 256, seen, &totals)
	WalkLargeStrings(raw2, 256, seen, &totals)
	if totals.RepeatBytes != int64(len(shared)) {
		t.Fatalf("repeat bytes = %d, want %d", totals.RepeatBytes, len(shared))
	}
	if totals.StringBytes != int64(len(shared)*2+400) {
		t.Fatalf("string bytes = %d", totals.StringBytes)
	}
	if totals.LargeStringShare() <= 0 || totals.RepeatShare() <= 0 {
		t.Fatalf("shares must be positive: %v %v", totals.LargeStringShare(), totals.RepeatShare())
	}
}

func TestWalkLargeStringsIgnoresSmallValuesAndKeys(t *testing.T) {
	raw := []byte(`{"kind":"record","note":"a short value"}`)
	seen := map[[16]byte]struct{}{}
	var totals DupTotals
	WalkLargeStrings(raw, 256, seen, &totals)
	if totals.StringBytes != 0 {
		t.Fatalf("string bytes = %d, want 0", totals.StringBytes)
	}
	if totals.RawBytes != int64(len(raw)) {
		t.Fatalf("raw bytes = %d, want %d", totals.RawBytes, len(raw))
	}
}

func TestToolOutputClassification(t *testing.T) {
	cases := []struct {
		agent, kind, role, tool, payload, metadata string
		want                                       bool
	}{
		{"pi", "message", "toolResult", "", "", "", true},
		{"pi", "message", "assistant", "", "", "", false},
		{"codex", "response_item", "", "", "function_call_output", "", true},
		{"codex", "response_item", "", "", "custom_tool_call_output", "", true},
		{"codex", "response_item", "", "", "function_call", "", false},
		{"codex", "response_item", "", "Bash", "", "", false},
		{"codex", "response_item", "assistant", "", "", "", false},
		{"codex", "response_item", "", "", "reasoning", "", false},
		{"claude-code", "user", "user", "", "", `{"blocks":[{"type":"tool_result","tool_use_id":"t1"}]}`, true},
		{"claude-code", "user", "user", "", "", `{"blocks":[{"type":"text","text":"hi"}]}`, false},
		{"claude-code", "user", "user", "", "", `{"toolUseResult":{"stdout":"x"}}`, true},
		{"claude-code", "assistant", "assistant", "Bash", "", `{"blocks":[{"type":"tool_use","name":"Bash"}]}`, false},
		{"claude-code", "user", "user", "", "", `{"contentForm":"string"}`, false},
	}
	for _, c := range cases {
		if got := ToolOutput(c.agent, c.kind, c.role, c.tool, c.payload, c.metadata); got != c.want {
			t.Errorf("ToolOutput(%q,%q,%q,%q,%q,md) = %v, want %v",
				c.agent, c.kind, c.role, c.tool, c.payload, got, c.want)
		}
	}
}

func TestSplitHalvesCoversAllRecords(t *testing.T) {
	records := [][]byte{
		[]byte(strings.Repeat("a", 10)),
		[]byte(strings.Repeat("b", 20)),
		[]byte(strings.Repeat("c", 30)),
		[]byte(strings.Repeat("d", 40)),
	}
	left, right := SplitHalves(records)
	if len(left)+len(right) != len(records) {
		t.Fatalf("halves lost records: %d + %d != %d", len(left), len(right), len(records))
	}
	var leftSum, rightSum int
	for _, r := range left {
		leftSum += len(r)
	}
	for _, r := range right {
		rightSum += len(r)
	}
	if leftSum != 60 {
		t.Fatalf("left half = %d bytes, want 60", leftSum)
	}
	if rightSum != 40 {
		t.Fatalf("right half = %d bytes, want 40", rightSum)
	}
	if string(left[0]) != strings.Repeat("a", 10) {
		t.Fatalf("left half reordered records")
	}
}

func TestDictSamplesCapsCountAndSize(t *testing.T) {
	var records [][]byte
	for i := 0; i < 100; i++ {
		records = append(records, []byte(strings.Repeat("x", 100+i)))
	}
	records = append(records, []byte(strings.Repeat("y", 200000)))
	got := DictSamples(records, 10, 64000)
	if len(got) != 10 {
		t.Fatalf("samples = %d, want 10", len(got))
	}
	for _, s := range got {
		if len(s) > 64000 {
			t.Fatalf("oversized sample kept")
		}
	}
}

func TestTrainDictionaryProducesUsableDict(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd CLI not available")
	}
	var records [][]byte
	for i := 0; i < 300; i++ {
		records = append(records, []byte(`{"n":`+strconv.Itoa(i)+`,"payload":"`+large(1200)+`","kind":"record"}`))
	}
	dict, err := TrainDictionary(t.TempDir(), DictSamples(records, 300, 64000), 4096)
	if err != nil {
		t.Fatalf("TrainDictionary: %v", err)
	}
	if len(dict) == 0 {
		t.Fatalf("empty dictionary")
	}
	if len(dict) > 8192 {
		t.Fatalf("dictionary larger than requested budget: %d", len(dict))
	}
}

func TestReadOnlyDSNShape(t *testing.T) {
	got := ReadOnlyDSN("/tmp/hub.db")
	if !strings.HasPrefix(got, "file:///tmp/hub.db?") {
		t.Fatalf("dsn = %q", got)
	}
	if !strings.Contains(got, "mode=ro") {
		t.Fatalf("dsn = %q, want mode=ro", got)
	}
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLargestSessionsAndReadSessionRaw(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`
		CREATE TABLE sessions (id TEXT PRIMARY KEY, device_id TEXT, agent TEXT, event_count INTEGER);
		CREATE TABLE events (id INTEGER PRIMARY KEY, session_id TEXT, raw_json TEXT);
		INSERT INTO sessions VALUES ('s1', 'd1', 'codex', 3), ('s2', 'd1', 'pi', 1);
		INSERT INTO events VALUES (1, 's1', '{"a":1}'), (2, 's1', '{"b":2}'), (3, 's1', '{"c":3}'), (4, 's2', '{"d":4}');
	`); err != nil {
		t.Fatal(err)
	}
	big, err := LargestSessions(db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(big) != 1 || big[0].ID != "s1" || big[0].EventCount != 3 {
		t.Fatalf("largest = %+v", big)
	}
	recs, got, err := ReadSessionRaw(db, "s1", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got != 21 || len(recs) != 3 {
		t.Fatalf("got = %d bytes, %d records", got, len(recs))
	}
	recs, got, err = ReadSessionRaw(db, "s1", 8)
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 || len(recs) != 1 {
		t.Fatalf("capped read = %d bytes, %d records", got, len(recs))
	}
}
