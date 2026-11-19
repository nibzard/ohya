package events

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/fixgen"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

var benchObserveOnce sync.Once
var benchObserveSink observe.Observer = observe.Disabled

func benchObserver() observe.Observer {
	benchObserveOnce.Do(func() {
		if os.Getenv("OHYA_BENCH_OBSERVE") != "on" {
			return
		}
		clock := observe.NewSystemClock()
		instance, err := observe.NewInstance("bench", "", false, clock, observe.RoleIngest)
		if err != nil {
			panic(err)
		}
		recorder, err := observe.NewRecorder(instance, clock)
		if err != nil {
			panic(err)
		}
		benchObserveSink = recorder
	})
	return benchObserveSink
}

func benchMIB() int {
	var n int
	if v := os.Getenv("OHYA_BENCH_MIB"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 32
}

func corpus(mib int) []fixgen.File {
	return fixgen.Generate(fixgen.Corpus{
		Sessions:            mib / 3,
		MainMeanBytes:       2 << 20,
		SubagentsPerSession: 2,
		Seed:                7,
	})
}

func corpusCodex(mib int) []fixgen.File {
	return fixgen.GenerateCodex(fixgen.Corpus{
		Sessions:            mib * 2 / 5,
		MainMeanBytes:       2 << 20,
		SubagentsPerSession: 2,
		Seed:                7,
	})
}

func corpusPi(mib int) []fixgen.File {
	return fixgen.GeneratePi(fixgen.Corpus{
		Sessions:      mib / 2,
		MainMeanBytes: 2 << 20,
		Seed:          7,
	})
}

const importMixMiB = 4384

func corpusMix(mib int) []fixgen.AgentFiles {
	return fixgen.GenerateMix(fixgen.ImportMix(float64(mib)/importMixMiB, 7))
}

func corpusBytes(files []fixgen.File) int64 {
	var n int64
	for _, f := range files {
		n += int64(len(f.Data))
	}
	return n
}

func openBenchHub(b *testing.B) *store.DB {
	b.Helper()
	hub, err := store.Open(context.Background(), filepath.Join(b.TempDir(), "hub.db"),
		store.WithObserver(benchObserver()))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { hub.Close() })
	return hub
}

func benchIngestFile(b *testing.B, hub *store.DB, agent string, f fixgen.File, sourceID string) error {
	const maxBatchBytes = 8 << 20
	ctx := context.Background()
	if err := hub.RegisterSource(ctx, sourceID, "dev-bench", agent, "/"+f.Path); err != nil {
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
		res, err := Parse(agent, f.Data[cursor:readEnd], cursor, opts)
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
			readEnd = min(end, readEnd+maxBatchBytes)
		} else {
			break
		}
	}
	return nil
}

func BenchmarkIngestCorpus(b *testing.B) {
	benchIngestCorpus(b, claudecode.AgentName, corpus)
}

func BenchmarkIngestCorpusCodex(b *testing.B) {
	benchIngestCorpus(b, codex.AgentName, corpusCodex)
}

func BenchmarkIngestCorpusPi(b *testing.B) {
	benchIngestCorpus(b, pi.AgentName, corpusPi)
}

func benchIngestCorpus(b *testing.B, agent string, gen func(int) []fixgen.File) {
	for _, mib := range []int{4, benchMIB()} {
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			files := gen(mib)
			b.ReportAllocs()
			b.SetBytes(corpusBytes(files))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				hub := openBenchHub(b)
				b.StartTimer()
				for j, f := range files {
					if err := benchIngestFile(b, hub, agent, f, fmt.Sprintf("src-%03d", j)); err != nil {
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

func BenchmarkIngestCorpusMix(b *testing.B) {
	for _, mib := range []int{4, benchMIB()} {
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			corpora := corpusMix(mib)
			var total int64
			for _, af := range corpora {
				for _, f := range af.Files {
					total += int64(len(f.Data))
				}
			}
			b.ReportAllocs()
			b.SetBytes(total)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				hub := openBenchHub(b)
				b.StartTimer()
				for _, af := range corpora {
					for j, f := range af.Files {
						sourceID := fmt.Sprintf("%s-%04d", af.Agent, j)
						if err := benchIngestFile(b, hub, af.Agent, f, sourceID); err != nil {
							b.Fatal(err)
						}
					}
				}
				b.StopTimer()
				hub.Close()
				b.StartTimer()
			}
		})
	}
}

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

func BenchmarkIngestReplay(b *testing.B) {
	ctx := context.Background()
	hub := openBenchHub(b)
	f := corpus(4)[0]
	if err := benchIngestFile(b, hub, claudecode.AgentName, f, "src-replay"); err != nil {
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

func BenchmarkIngestAppend(b *testing.B) {
	ctx := context.Background()
	const sourceID = "src-append"
	f := fixgen.Generate(fixgen.Corpus{Sessions: 1, MainMeanBytes: 16 << 20, Seed: 11})[0]
	res, err := Parse(claudecode.AgentName, f.Data, 0, Options{SourceScope: sourceID})
	if err != nil {
		b.Fatal(err)
	}
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
