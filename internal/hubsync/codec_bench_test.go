package hubsync

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/nibzard/ohya/internal/fixgen"
)

type codecBenchAgent struct {
	name     string
	batches  []Batch
	sections [][]byte
	streams  [][]byte
	raw      int64
	comp     int64
}

type codecBenchCorpus struct {
	agents []*codecBenchAgent
}

var mixCodecCorpus = sync.OnceValue(func() *codecBenchCorpus {
	corpora := fixgen.GenerateMix(fixgen.ImportMix(fixgen.ImportScale, 7))
	out := &codecBenchCorpus{}
	for _, af := range corpora {
		a := &codecBenchAgent{name: af.Agent}
		for _, f := range af.Files {
			b := recordsBatch(f.Data)
			section := make([]byte, 0, int64(len(b.Records))*batchRecordFrameBytes+b.End-b.Start)
			section = appendRecordSection(section, b)
			stream := batchEncoder().EncodeAll(section, nil)
			a.batches = append(a.batches, b)
			a.sections = append(a.sections, section)
			a.streams = append(a.streams, stream)
			a.raw += int64(len(section))
			a.comp += int64(len(stream))
		}
		out.agents = append(out.agents, a)
	}
	return out
})

func recordsBatch(data []byte) Batch {
	b := Batch{DeviceID: "bench-device", SourceID: "bench-source"}
	off := int64(0)
	for off < int64(len(data)) {
		idx := bytes.IndexByte(data[off:], '\n')
		if idx < 0 {
			break
		}
		rec := Record{Offset: off, JSON: data[off : off+int64(idx) : off+int64(idx)]}
		if b.Records == nil {
			b.Start = rec.Offset
		}
		b.Records = append(b.Records, rec)
		b.End = rec.End()
		off = rec.End()
	}
	return b
}

func (c *codecBenchCorpus) agent(name string) *codecBenchAgent {
	for _, a := range c.agents {
		if a.name == name {
			return a
		}
	}
	return nil
}

func benchAgents() []string {
	return []string{"claude-code", "codex", "pi"}
}

func BenchmarkBatchWritePlain(b *testing.B) {
	corpus := mixCodecCorpus()
	for _, name := range benchAgents() {
		b.Run("agent="+name, func(b *testing.B) {
			a := corpus.agent(name)
			if a == nil || len(a.batches) == 0 {
				b.Fatalf("no batches for agent %s", name)
			}
			w := io.Discard
			b.SetBytes(a.raw)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, bt := range a.batches {
					if err := WriteBatch(w, bt); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func BenchmarkBatchEncode(b *testing.B) {
	corpus := mixCodecCorpus()
	for _, speed := range []struct {
		name  string
		level int
	}{
		{"fastest", 1},
		{"default", 3},
		{"better", 7},
		{"best", 11},
	} {
		b.Run("speed="+speed.name, func(b *testing.B) {
			enc, err := zstd.NewWriter(nil,
				zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(speed.level)),
				zstd.WithEncoderCRC(true))
			if err != nil {
				b.Fatal(err)
			}
			defer enc.Close()
			for _, name := range benchAgents() {
				b.Run("agent="+name, func(b *testing.B) {
					a := corpus.agent(name)
					if a == nil || len(a.sections) == 0 {
						b.Fatalf("no sections for agent %s", name)
					}
					buf := make([]byte, 0, 1<<20)
					var comp int64
					b.SetBytes(a.raw)
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						comp = 0
						for _, s := range a.sections {
							buf = enc.EncodeAll(s, buf[:0])
							comp += int64(len(buf))
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(a.raw)/float64(comp), "x-ratio")
				})
			}
		})
	}
}

func BenchmarkBatchDecode(b *testing.B) {
	corpus := mixCodecCorpus()
	dec := batchDecoder()
	for _, name := range benchAgents() {
		b.Run("agent="+name, func(b *testing.B) {
			a := corpus.agent(name)
			if a == nil || len(a.streams) == 0 {
				b.Fatalf("no streams for agent %s", name)
			}
			dsts := make([][]byte, len(a.streams))
			for i, s := range a.streams {
				dsts[i] = make([]byte, 0, cap(a.sections[i]))
				var err error
				dsts[i], err = dec.DecodeAll(s, dsts[i])
				if err != nil {
					b.Fatal(err)
				}
			}
			b.SetBytes(a.raw)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j, s := range a.streams {
					if _, err := dec.DecodeAll(s, dsts[j][:0]); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func TestCodecBenchCorpusShapes(t *testing.T) {
	corpus := mixCodecCorpus()
	var raw, comp int64
	files := 0
	for _, a := range corpus.agents {
		if len(a.batches) == 0 || len(a.batches) != len(a.sections) || len(a.batches) != len(a.streams) {
			t.Fatalf("agent %s holds %d batches, %d sections, %d streams", a.name, len(a.batches), len(a.sections), len(a.streams))
		}
		for i, bt := range a.batches {
			var jsonBytes int64
			for _, r := range bt.Records {
				jsonBytes += int64(len(r.JSON))
			}
			want := jsonBytes + int64(len(bt.Records))*batchRecordFrameBytes
			if int64(len(a.sections[i])) != want {
				t.Fatalf("agent %s batch %d: section %d bytes, want %d", a.name, i, len(a.sections[i]), want)
			}
			if !bytes.Equal(bt.Records[0].JSON[:1], []byte{'{'}) {
				t.Fatalf("agent %s batch %d: first record is not JSON", a.name, i)
			}
		}
		raw += a.raw
		comp += a.comp
		files += len(a.batches)
	}
	if raw < 100<<20 {
		t.Fatalf("corpus raw section bytes = %d; want the full import fixture (over 100 MiB)", raw)
	}
	if comp == 0 || comp >= raw {
		t.Fatalf("corpus compressed bytes = %d of raw %d; want real compression", comp, raw)
	}
	t.Logf("corpus: %d files, %d raw section bytes, level-3 ratio %.2f", files, raw, float64(raw)/float64(comp))
}
