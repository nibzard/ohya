package pi

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// FuzzParse holds the parser's structural guarantees against arbitrary
// bytes: contiguous boundaries, verbatim raw text, one native ID per source,
// a cursor that only lands on accepted record boundaries, deterministic
// output, prefix stability for any cut of the input, and split stability (a
// later poll resuming at the cursor with the established native reproduces
// the full parse).
func FuzzParse(f *testing.F) {
	fixtures, err := filepath.Glob(filepath.Join("..", "..", "testdata", "pi", "*.jsonl"))
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range fixtures {
		f.Add(fixture(f, filepath.Base(path)))
	}
	f.Add([]byte(`{"type":"session","version":3,"id":"s1","timestamp":"2026-11-13T09:00:00.000Z","cwd":"/w"}` + "\n" +
		`{"type":"message","id":"ab000001","parentId":null,"timestamp":"2026-11-13T09:00:01.000Z","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"ls"}}],"usage":{"input":1,"output":2,"cacheRead":0,"totalTokens":3},"responseId":"r1"}}` + "\n" +
		`{"type":"message"`))
	f.Add([]byte(`{"type":"message","id":"ab000002","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n" +
		`{"type":"session","version":3,"id":"late","cwd":"/w"}` + "\n"))
	f.Add([]byte("{\"type\":\"brand-new\"}\n{}\n[]\nnull\n\n{\"a\":1}{\"b\":2}\n"))
	f.Add([]byte(`{"type":"message","id":"ab000003","message":{"role":"assistant","usage":{"input":-1,"output":0,"totalTokens":-1},"responseId":""}}` + "\n"))
	f.Add([]byte("{bad json\n{\"type\":\"message\"}\n"))
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

			if err == nil {
				// Without an error every newline was consumed: the
				// remaining bytes are genuinely the incomplete tail.
				if bytes.IndexByte(data[res.Cursor:], '\n') >= 0 {
					t.Fatalf("cursor %d leaves an unparsed newline", res.Cursor)
				}
			} else {
				// With an error the source stops exactly at the cursor,
				// never past accepted bytes and never skipping any.
				var perr *ParseError
				if !errors.As(err, &perr) {
					t.Fatalf("error is not a ParseError: %v", err)
				}
				if perr.Offset != res.Cursor {
					t.Fatalf("error offset %d, want cursor %d", perr.Offset, res.Cursor)
				}
			}

			for _, cut := range []int{0, len(data) / 4, len(data) / 2, int(res.Cursor), 3 * len(data) / 4, len(data)} {
				// Prefix stability: any cut yields a prefix of the same
				// records and a cursor inside the cut.
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

				// Split stability: resuming at the cut's cursor with the
				// established native reproduces the full parse.
				if subRes.Cursor == 0 {
					continue
				}
				ro := o
				ro.Native = subRes.Native
				rest, restErr := Parse(data[subRes.Cursor:], subRes.Cursor, ro)
				checkInvariants(t, data[subRes.Cursor:], subRes.Cursor, rest)
				joined := append(append([]Record{}, subRes.Records...), rest.Records...)
				if !reflect.DeepEqual(joined, res.Records) || rest.Cursor != res.Cursor {
					t.Fatalf("resuming at %d changed the records", subRes.Cursor)
				}
				if (restErr == nil) != (err == nil) {
					t.Fatalf("resuming at %d changed the outcome: %v versus %v", subRes.Cursor, restErr, err)
				}
			}
		}
	})
}
