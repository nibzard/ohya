// Ingestion benchmarks over a deterministic synthetic corpus (internal/fixgen).
// They measure the full local path — events.Parse plus the transactional
// write path — with the same 8 MiB batching cmd/ohya uses, so the numbers
// answer: how fast does a first import run, what does an idempotent replay
// cost, and what does one live append (one poll's new bytes) cost? The
// companion measurements and method live in docs/performance.md.

package events

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

// benchMIB is the benchmark corpus volume, from OHYA_BENCH_MIB. The default
// keeps a full benchmark run in the tens of seconds; the documented
// measurements raise it (docs/performance.md).
func benchMIB() int {
	var n int
	if v := os.Getenv("OHYA_BENCH_MIB"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 32
}

// corpus returns a benchmark corpus of about mib MiB: sessions of about
// 2 MiB (the verified real-corpus mean is 2.3 MB per main session), each
// with a mean of two subagent transcripts of a quarter size, so one session
// group averages 3 MiB.
func corpus(mib int) []fixgen.File {
	return fixgen.Generate(fixgen.Corpus{
		Sessions:            mib / 3,
		MainMeanBytes:       2 << 20,
		SubagentsPerSession: 2,
		Seed:                7,
	})
}

// corpusCodex returns a Codex rollout corpus of about mib MiB with the same
// shape: main rollouts of about 2 MiB, each with a mean of two subagent
// rollouts of a quarter size (one session group averages 2.5 MiB).
func corpusCodex(mib int) []fixgen.File {
	return fixgen.GenerateCodex(fixgen.Corpus{
		Sessions:            mib * 2 / 5,
		MainMeanBytes:       2 << 20,
		SubagentsPerSession: 2,
		Seed:                7,
	})
}

// corpusPi returns a Pi session corpus of about mib MiB: one file per
// session of about 2 MiB — Pi sessions never span files and carry no
// subagent transcripts.
func corpusPi(mib int) []fixgen.File {
	return fixgen.GeneratePi(fixgen.Corpus{
		Sessions:      mib / 2,
		MainMeanBytes: 2 << 20,
		Seed:          7,
	})
}

func corpusBytes(files []fixgen.File) int64 {
	var n int64
	for _, f := range files {
		n += int64(len(f.Data))
	}
	return n
}

// openBenchHub opens a fresh hub database under a benchmark-owned directory.
func openBenchHub(b *testing.B) *store.DB {
	b.Helper()
	hub, err := store.Open(context.Background(), filepath.Join(b.TempDir(), "hub.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { hub.Close() }) // Close is idempotent after explicit closes
	return hub
}

// benchIngestFile feeds one file through the local ingestion path with cmd/ohya's
// exact batching: 8 MiB reads, parse, one transaction per batch, SourceState
// before parses past offset 0. All benchmark corpus records are valid, so a
// parse error is a benchmark failure, not a source stop.
func benchIngestFile(b *testing.B, hub *store.DB, f fixgen.File, sourceID string) error {
	const maxBatchBytes = 8 << 20 // mirrors cmd/ohya maxBatchBytes
	ctx := context.Background()
	if err := hub.RegisterSource(ctx, sourceID, "dev-bench", claudecode.AgentName, "/"+f.Path); err != nil {
		return err
	}
	end := int64(len(f.Data))
	cursor, readEnd := int64(0), min(end, maxBatchBytes)
	for cursor < end {
		opts := Options{SourceScope: sourceID}
		if cursor > 0 {
			st, err := hub.SourceState(ctx, sourceID)
			if err != nil {
				return err
			}
			opts.FirstNative = st.FirstNativeID
		}
		res, err := Parse(claudecode.AgentName, f.Data[cursor:readEnd], cursor, opts)
		if err != nil {
			return err
		}
		if len(res.Events) > 0 {
			result, err := hub.Ingest(ctx, store.Batch{
				SourceID: sourceID, DeviceID: "dev-bench", Events: res.Events})
			if err != nil {
				return err
			}
			cursor = result.Cursor
			readEnd = min(end, cursor+maxBatchBytes)
		} else if readEnd < end {
			// One record larger than the batch bound: extend the read.
			readEnd = min(end, readEnd+maxBatchBytes)
		} else {
			break // one incomplete record still growing
		}
	}
	return nil
}

// BenchmarkIngestCorpus measures a first import: fresh database, every file
// through the full path. Each iteration gets a fresh database, so the rate
// includes store.Open (migrations) but is dominated by parse, insert,
// aggregate and FTS work.
func BenchmarkIngestCorpus(b *testing.B) {
	for _, mib := range []int{4, benchMIB()} {
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			files := corpus(mib)
			b.ReportAllocs()
			b.SetBytes(corpusBytes(files))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				hub := openBenchHub(b)
				b.StartTimer()
				for j, f := range files {
					if err := benchIngestFile(b, hub, f, fmt.Sprintf("src-%03d", j)); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				hub.Close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkParseCorpus isolates the parser: the same corpus bytes, no
// database. The gap to BenchmarkIngestCorpus is the write path's share.
// The Codex and Pi twins below hold the per-agent parse cost against the
// shared write path: their corpora draw texts from the same distribution,
// so the delta reflects the record envelope and parser, not text volume.
func BenchmarkParseCorpus(b *testing.B) {
	benchParseCorpus(b, claudecode.AgentName, corpus(benchMIB()))
}

func BenchmarkParseCorpusCodex(b *testing.B) {
	benchParseCorpus(b, codex.AgentName, corpusCodex(benchMIB()))
}

func BenchmarkParseCorpusPi(b *testing.B) {
	benchParseCorpus(b, pi.AgentName, corpusPi(benchMIB()))
}

func benchParseCorpus(b *testing.B, agent string, files []fixgen.File) {
	b.ReportAllocs()
	b.SetBytes(corpusBytes(files))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, f := range files {
			if _, err := Parse(agent, f.Data, 0,
				Options{SourceScope: "bench-parse"}); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkIngestReplay measures a fully repeated batch: every record sits
// below the committed cursor, so the write path validates stored bytes and
// commits nothing. This is the cost of a retry after a lost acknowledgment
// and of the uploader's per-connection cursor probe.
func BenchmarkIngestReplay(b *testing.B) {
	ctx := context.Background()
	hub := openBenchHub(b)
	f := corpus(4)[0]
	if err := benchIngestFile(b, hub, f, "src-replay"); err != nil {
		b.Fatal(err)
	}
	res, err := Parse(claudecode.AgentName, f.Data, 0, Options{SourceScope: "src-replay"})
	if err != nil {
		b.Fatal(err)
	}
	batch := store.Batch{SourceID: "src-replay", DeviceID: "dev-bench", Events: res.Events}
	b.ReportAllocs()
	b.SetBytes(int64(len(f.Data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := hub.Ingest(ctx, batch)
		if err != nil {
			b.Fatal(err)
		}
		if result.Inserted != 0 {
			b.Fatalf("replay inserted %d events", result.Inserted)
		}
	}
}

// BenchmarkIngestAppend measures one live append: eight records (about one
// turn) parsed and committed on top of a 16 MiB source, the way a poll
// delivers a running session's new bytes. FULL synchronous durability makes
// the commit, not the parse, the dominant cost. The corpus holds a bounded
// number of append windows, so each completed lap re-primes a fresh database
// holding only the committed prefix: appends must sit above a real cursor,
// and the benchmark must serve any b.N the framework asks for — fast storage
// drives b.N past one lap within the default benchtime.
func BenchmarkIngestAppend(b *testing.B) {
	ctx := context.Background()
	const sourceID = "src-append"
	f := fixgen.Generate(fixgen.Corpus{Sessions: 1, MainMeanBytes: 16 << 20, Seed: 11})[0]
	res, err := Parse(claudecode.AgentName, f.Data, 0, Options{SourceScope: sourceID})
	if err != nil {
		b.Fatal(err)
	}
	// Commit a prefix once; the appends continue exactly at its cursor.
	const prefix = 1024
	const per = 8
	if len(res.Events) <= prefix+per {
		b.Fatal("session too short for append windows")
	}
	windows := (len(res.Events) - prefix) / per

	dir, err := os.MkdirTemp("", "ohya-append-bench-")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "hub.db")
	var hub *store.DB
	// reprime replaces the database with a fresh one that holds only the
	// committed prefix, so the next lap appends above a real cursor again.
	reprime := func() {
		if hub != nil {
			hub.Close()
		}
		for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				b.Fatal(err)
			}
		}
		hub, err = store.Open(ctx, dbPath)
		if err == nil {
			err = hub.RegisterSource(ctx, sourceID, "dev-bench", claudecode.AgentName,
				"/"+f.Path)
		}
		if err == nil {
			_, err = hub.Ingest(ctx, store.Batch{SourceID: sourceID, DeviceID: "dev-bench",
				Events: res.Events[:prefix]})
		}
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%windows == 0 {
			b.StopTimer()
			reprime()
			b.StartTimer()
		}
		first := prefix + (i%windows)*per
		off, end := res.Events[first].Offset, res.Events[first+per-1].End
		parsed, err := Parse(claudecode.AgentName, f.Data[off:end], off,
			Options{SourceScope: sourceID})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := hub.Ingest(ctx, store.Batch{
			SourceID: sourceID, DeviceID: "dev-bench", Events: parsed.Events}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	hub.Close()
}
