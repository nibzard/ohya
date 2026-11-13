package claudecode

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// FuzzParse holds the parser's structural guarantees against arbitrary
// bytes: contiguous boundaries, verbatim raw text, a cursor that only lands
// on accepted record boundaries, deterministic output, and prefix stability
// for any cut of the input (an interrupted or partial read must never
// change what the earlier records mean).
func FuzzParse(f *testing.F) {
	fixtures, err := filepath.Glob(filepath.Join("..", "..", "testdata", "claude-code", "*.jsonl"))
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range fixtures {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte("{\"type\":\"user\",\"sessionId\":\"s\",\"message\":{\"role\":\"user\",\"content\":\"hi\"}}\n{\"type\":\"assistant\""))
	f.Add([]byte("{\"type\":\"assistant\",\"sessionId\":\"s\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"name\":\"T\",\"input\":{\"a\":[1,2]}},{\"type\":\"text\",\"text\":\"x\"}]}}\n"))
	f.Add([]byte("{\"type\":\"brand-new\",\"sessionId\":\"s\"}\n{\"sessionId\":\"s\"}\n[]\nnull\n\n{\"a\":1}{\"b\":2}\n"))
	f.Add([]byte("{\"type\":\"user\",\"timestamp\":\"2026-11-13T09:00:01.000Z\",\"message\":{\"content\":\"p\"}}\n"))
	f.Add([]byte("{bad json\n{\"sessionId\":\"s\"}\n"))
	f.Add(bytes.Repeat([]byte("a"), 300))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, limit := range []int64{16, 64, DefaultMaxRecordBytes} {
			o := Options{SourceScope: "fuzz-scope", MaxRecordBytes: limit}
			res, err := Parse(data, 0, o)
			checkInvariants(t, data, 0, res)

			// Determinism: identical inputs produce identical outputs.
			res2, err2 := Parse(data, 0, o)
			if !reflect.DeepEqual(res, res2) || !reflect.DeepEqual(err, err2) {
				t.Fatal("parse is not deterministic")
			}

			// Without an error every newline was consumed: the remaining
			// bytes are genuinely the incomplete tail.
			if err == nil {
				if bytes.IndexByte(data[res.Cursor:], '\n') >= 0 {
					t.Fatalf("cursor %d leaves an unparsed newline", res.Cursor)
				}
				continue
			}

			// With an error the source stops exactly at the cursor, never
			// past accepted bytes and never silently skipping any.
			var perr *ParseError
			if !errors.As(err, &perr) {
				t.Fatalf("error is not a ParseError: %v", err)
			}
			if perr.Offset != res.Cursor {
				t.Fatalf("error offset %d, want cursor %d", perr.Offset, res.Cursor)
			}

			// Prefix stability: any cut of the input yields a prefix of
			// the same records and a cursor inside the cut.
			for _, cut := range []int{0, len(data) / 4, len(data) / 2, int(res.Cursor), 3 * len(data) / 4, len(data)} {
				sub := data[:cut]
				subRes, _ := Parse(sub, 0, o)
				checkInvariants(t, sub, 0, subRes)
				if len(subRes.Records) > len(res.Records) {
					t.Fatalf("cut %d produced more records (%d) than the full input (%d)", cut, len(subRes.Records), len(res.Records))
				}
				for i := range subRes.Records {
					if !reflect.DeepEqual(subRes.Records[i], res.Records[i]) {
						t.Fatalf("cut %d changed record %d", cut, i)
					}
				}
				if subRes.Cursor > int64(cut) {
					t.Fatalf("cut %d cursor %d exceeds the cut", cut, subRes.Cursor)
				}
			}
		}
	})
}
