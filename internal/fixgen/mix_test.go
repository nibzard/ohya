package fixgen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

const mixPinnedHash = "cf951188e411bf95b5ef6adf0194d82eb74eb0164ed0e6babe740299d2c89ab9"

func filesHash(files []File) string {
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestGenerateMixIsDeterministic(t *testing.T) {
	m := Mix{Seed: 7, Scale: 1.0 / 256, Compact: true}
	first, second := GenerateMix(m), GenerateMix(m)
	if len(first) != len(second) {
		t.Fatalf("agent corpus count changed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Agent != second[i].Agent {
			t.Fatalf("agent %d changed: %s vs %s", i, first[i].Agent, second[i].Agent)
		}
		compareCorpora(t, first[i].Files, second[i].Files)
	}
	if got := filesHash(agentFilesSlice(first)); got != mixPinnedHash {
		t.Fatalf("mix corpus hash %s, want %s; seed %d no longer reproduces the pinned fixture bytes",
			got, mixPinnedHash, m.Seed)
	}
}

func agentFilesSlice(corpora []AgentFiles) []File {
	var files []File
	for _, af := range corpora {
		files = append(files, af.Files...)
	}
	return files
}

func TestImportMixMatchesObservedShape(t *testing.T) {
	m := ImportMix(1.0/256, 7)
	corpora := GenerateMix(m)
	specs := m.specs()
	agents := []string{claudecode.AgentName, codex.AgentName, pi.AgentName}
	wantSessions := []int{4, 6, 1}
	for i, af := range corpora {
		if af.Agent != agents[i] {
			t.Fatalf("agent corpus %d is %s, want %s", i, af.Agent, agents[i])
		}
		if specs[i].sessions != wantSessions[i] {
			t.Fatalf("%s: scale %g produced %d sessions, want %d", af.Agent, m.Scale, specs[i].sessions, wantSessions[i])
		}
		if len(af.Files) == 0 {
			t.Fatalf("%s: no files generated", af.Agent)
		}
		for _, f := range af.Files {
			if len(f.Data) == 0 || f.Data[len(f.Data)-1] != '\n' {
				t.Fatalf("%s: %s is not newline-terminated", af.Agent, f.Path)
			}
		}
	}
	if len(corpora[0].Files) <= specs[0].sessions {
		t.Fatalf("claude-code corpus holds %d files for %d sessions; the observed fan-out carries subagent transcripts",
			len(corpora[0].Files), specs[0].sessions)
	}
	if len(corpora[1].Files) <= specs[1].sessions {
		t.Fatalf("codex corpus holds %d files for %d sessions; the observed fan-out carries subagent rollouts",
			len(corpora[1].Files), specs[1].sessions)
	}
	if len(corpora[2].Files) != specs[2].sessions {
		t.Fatalf("pi corpus holds %d files for %d sessions; pi has no subagent transcripts",
			len(corpora[2].Files), specs[2].sessions)
	}
}

func TestMixParsesAndIngests(t *testing.T) {
	ctx := context.Background()
	hub, err := store.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	corpora := GenerateMix(ImportMix(1.0/256, 7))
	inserted := 0
	var lastAgent, lastSource string
	var lastFile File
	for _, af := range corpora {
		for j, f := range af.Files {
			sourceID := fmt.Sprintf("%s-src-%03d", af.Agent, j)
			if err := hub.RegisterSource(ctx, sourceID, "dev-fixgen", af.Agent, "/"+f.Path); err != nil {
				t.Fatal(err)
			}
			res, err := events.Parse(af.Agent, f.Data, 0, events.Options{SourceScope: sourceID})
			if err != nil {
				t.Fatalf("%s: %s: parse: %v", af.Agent, f.Path, err)
			}
			if res.Cursor != int64(len(f.Data)) {
				t.Fatalf("%s: cursor %d != file size %d", f.Path, res.Cursor, len(f.Data))
			}
			result, err := hub.Ingest(ctx, store.Batch{
				SourceID: sourceID, DeviceID: "dev-fixgen", Events: res.Events})
			if err != nil {
				t.Fatalf("%s: ingest: %v", f.Path, err)
			}
			inserted += result.Inserted
			lastAgent, lastSource, lastFile = af.Agent, sourceID, f
		}
	}
	if inserted == 0 {
		t.Fatal("mix inserted no events")
	}
	res, err := events.Parse(lastAgent, lastFile.Data, 0, events.Options{SourceScope: lastSource})
	if err != nil {
		t.Fatal(err)
	}
	result, err := hub.Ingest(ctx, store.Batch{
		SourceID: lastSource, DeviceID: "dev-fixgen", Events: res.Events})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if result.Inserted != 0 {
		t.Fatalf("replay inserted %d events, want 0", result.Inserted)
	}
}

func TestMixCompactShrinksRecords(t *testing.T) {
	full := GenerateMix(Mix{Seed: 7, Scale: 1.0 / 256})
	compact := GenerateMix(Mix{Seed: 7, Scale: 1.0 / 256, Compact: true})
	fullStats, err := MeasureMix(Mix{Seed: 7, Scale: 1.0 / 256}, full)
	if err != nil {
		t.Fatal(err)
	}
	compactStats, err := MeasureMix(Mix{Seed: 7, Scale: 1.0 / 256, Compact: true}, compact)
	if err != nil {
		t.Fatal(err)
	}
	for i := range fullStats {
		if compactStats[i].Events == 0 {
			t.Fatalf("%s: compact corpus carries no events", fullStats[i].Agent)
		}
		if want := fullStats[i].MeanRecord * 3 / 5; compactStats[i].MeanRecord > want {
			t.Fatalf("%s: compact mean record %d exceeds 60%% of the full %d",
				fullStats[i].Agent, compactStats[i].MeanRecord, fullStats[i].MeanRecord)
		}
	}
}

func TestAgentCorpusStableHash(t *testing.T) {
	c := Corpus{Sessions: 3, MainMeanBytes: 64 << 10, SubagentsPerSession: 1, Seed: 7}
	cases := []struct {
		name  string
		files []File
		want  string
	}{
		{"claude-code", Generate(c), "8dd7315f320af895e6a5e520287c00c43e286a9e0eda01fdc52cca70bdf057d7"},
		{"codex", GenerateCodex(c), "5a501361f2a8dcd14416f20d444c4fc4f45f7ed5e7172d4fb19d1754af47ffd7"},
		{"pi", GeneratePi(Corpus{Sessions: 3, MainMeanBytes: 64 << 10, Seed: 7}),
			"3288ef37a7c0d81d208c888081303088fc984b36ed622a2cab5806db54632302"},
	}
	for _, tc := range cases {
		if got := filesHash(tc.files); got != tc.want {
			t.Errorf("%s corpus hash %s, want %s; the generator bytes changed, so the corpus measurements in docs/performance.md no longer hold — repin both together", tc.name, got, tc.want)
		}
	}
}

func TestNamedFixtureStats(t *testing.T) {
	if os.Getenv("OHYA_MIX_VERIFY") != "1" {
		t.Skip("set OHYA_MIX_VERIFY=1 to generate and measure the named fixtures (about 1 GiB of memory)")
	}
	for _, tc := range []struct {
		name                 string
		m                    Mix
		minEvents, maxEvents int64
	}{
		{"import", ImportMix(ImportScale, 7), 30_000, 60_000},
		{"query", QueryMix(7), 950_000, 1_050_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corpora := GenerateMix(tc.m)
			stats, err := MeasureMix(tc.m, corpora)
			if err != nil {
				t.Fatal(err)
			}
			var events, bytes, records int64
			var files, sessions int
			for _, s := range stats {
				t.Logf("%s sessions=%d files=%d records=%d events=%d bytes=%d mean-record=%d max-record=%d",
					s.Agent, s.Sessions, s.Files, s.Records, s.Events, s.Bytes, s.MeanRecord, s.MaxRecord)
				events += s.Events
				bytes += s.Bytes
				records += s.Records
				files += s.Files
				sessions += s.Sessions
			}
			t.Logf("total sessions=%d files=%d records=%d events=%d bytes=%d (%.1f MiB)",
				sessions, files, records, events, bytes, float64(bytes)/(1<<20))
			if events < tc.minEvents || events > tc.maxEvents {
				t.Fatalf("%s fixture holds %d events, want %d-%d", tc.name, events, tc.minEvents, tc.maxEvents)
			}
		})
	}
}
