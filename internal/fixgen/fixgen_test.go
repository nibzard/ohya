package fixgen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

func TestCorpusParsesCompletely(t *testing.T) {
	c := Corpus{Sessions: 8, MainMeanBytes: 256 << 10, SubagentsPerSession: 2}
	files := Generate(c)
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	var total int64
	for _, f := range files {
		res, err := claudecode.Parse(f.Data, 0, claudecode.Options{SourceScope: "fixgen-test"})
		if err != nil {
			t.Fatalf("%s: parse: %v", f.Path, err)
		}
		if res.Cursor != int64(len(f.Data)) {
			t.Fatalf("%s: cursor %d != file size %d", f.Path, res.Cursor, len(f.Data))
		}
		if _, err := events.Parse(claudecode.AgentName, f.Data, 0,
			events.Options{SourceScope: "fixgen-test"}); err != nil {
			t.Fatalf("%s: events.Parse: %v", f.Path, err)
		}
		if !strings.HasSuffix(f.Path, ".jsonl") {
			t.Fatalf("%s: not a session file name", f.Path)
		}
		total += int64(len(f.Data))
	}
	if min := int64(c.Sessions) * c.MainMeanBytes; total < min {
		t.Fatalf("corpus of %d bytes is smaller than its %d main sessions alone", total, min)
	}
}

func TestCodexCorpusParsesCompletely(t *testing.T) {
	c := Corpus{Sessions: 4, MainMeanBytes: 256 << 10, SubagentsPerSession: 2}
	files := GenerateCodex(c)
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	var total, prompts, usages, usageRecords int64
	for _, f := range files {
		res, err := events.Parse(codex.AgentName, f.Data, 0,
			events.Options{SourceScope: "fixgen-test"})
		if err != nil {
			t.Fatalf("%s: events.Parse: %v", f.Path, err)
		}
		if res.Cursor != int64(len(f.Data)) {
			t.Fatalf("%s: cursor %d != file size %d", f.Path, res.Cursor, len(f.Data))
		}
		for _, e := range res.Events {
			if e.Prompt != "" {
				prompts++
			}
			if e.Kind == "token_usage_record" {
				usageRecords++
			}
			if e.HasUsage {
				usages++
			}
		}
		total += int64(len(f.Data))
	}
	if min := int64(c.Sessions) * c.MainMeanBytes; total < min {
		t.Fatalf("corpus of %d bytes is smaller than its %d main rollouts alone", total, min)
	}
	if prompts == 0 {
		t.Fatal("corpus carries no prompts")
	}
	if usages != usageRecords {
		t.Fatalf("%d of %d usage records count in totals; the corpus violates a verified usage identity",
			usages, usageRecords)
	}
}

func TestPiCorpusParsesCompletely(t *testing.T) {
	c := Corpus{Sessions: 4, MainMeanBytes: 256 << 10}
	files := GeneratePi(c)
	if len(files) != c.Sessions {
		t.Fatalf("generated %d files, want one per session (%d)", len(files), c.Sessions)
	}
	var total, prompts, usages, assistants int64
	for _, f := range files {
		res, err := events.Parse(pi.AgentName, f.Data, 0,
			events.Options{SourceScope: "fixgen-test"})
		if err != nil {
			t.Fatalf("%s: events.Parse: %v", f.Path, err)
		}
		if res.Cursor != int64(len(f.Data)) {
			t.Fatalf("%s: cursor %d != file size %d", f.Path, res.Cursor, len(f.Data))
		}
		for _, e := range res.Events {
			if e.Prompt != "" {
				prompts++
			}
			if e.Kind == "message" && e.Role == "assistant" {
				assistants++
			}
			if e.HasUsage {
				usages++
			}
		}
		total += int64(len(f.Data))
	}
	if min := int64(c.Sessions) * c.MainMeanBytes; total < min {
		t.Fatalf("corpus of %d bytes is smaller than its %d sessions alone", total, min)
	}
	if prompts == 0 {
		t.Fatal("corpus carries no prompts")
	}
	if usages != assistants {
		t.Fatalf("%d of %d assistant responses carry counted usage; the corpus violates the per-response usage identity",
			usages, assistants)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	c := Corpus{Sessions: 3, MainMeanBytes: 64 << 10, SubagentsPerSession: 1}
	first, second := Generate(c), Generate(c)
	compareCorpora(t, first, second)
}

func TestWriteMirrorsProjectsLayout(t *testing.T) {
	root := t.TempDir()
	c := Corpus{Sessions: 2, MainMeanBytes: 8 << 10, SubagentsPerSession: 1}
	if _, err := Write(root, c); err != nil {
		t.Fatal(err)
	}
	var mains, subs int
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jsonl" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Errorf("%s: not newline-terminated", p)
		}
		if strings.Contains(p, "subagents") {
			subs++
		} else {
			mains++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if mains != c.Sessions {
		t.Fatalf("wrote %d main sessions, want %d", mains, c.Sessions)
	}
	if subs == 0 {
		t.Fatal("no subagent files written")
	}
}

func TestGenerateCodexIsDeterministic(t *testing.T) {
	c := Corpus{Sessions: 3, MainMeanBytes: 64 << 10, SubagentsPerSession: 1}
	first, second := GenerateCodex(c), GenerateCodex(c)
	compareCorpora(t, first, second)
}

func TestGeneratePiIsDeterministic(t *testing.T) {
	c := Corpus{Sessions: 3, MainMeanBytes: 64 << 10}
	first, second := GeneratePi(c), GeneratePi(c)
	compareCorpora(t, first, second)
}

func compareCorpora(t *testing.T, first, second []File) {
	t.Helper()
	if len(first) != len(second) {
		t.Fatalf("file count changed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Path != second[i].Path || string(first[i].Data) != string(second[i].Data) {
			t.Fatalf("file %d not deterministic", i)
		}
	}
}

var rolloutPath = regexp.MustCompile(`^[0-9]{4}/[0-9]{2}/[0-9]{2}/rollout-\S+\.jsonl$`)
var piPath = regexp.MustCompile(`^--[^/]+--/[0-9T:.-]+Z_[0-9a-f-]{36}\.jsonl$`)

func TestWriteCodexMirrorsRolloutLayout(t *testing.T) {
	root := t.TempDir()
	c := Corpus{Sessions: 3, MainMeanBytes: 8 << 10, SubagentsPerSession: 1}
	if _, err := WriteCodex(root, c); err != nil {
		t.Fatal(err)
	}
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jsonl" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Errorf("%s: not newline-terminated", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if !rolloutPath.MatchString(rel) {
			t.Errorf("%s: not a rollout path", rel)
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || len(files) > 2*c.Sessions {
		t.Fatalf("wrote %d rollout files for %d sessions", len(files), c.Sessions)
	}
}

func TestWritePiMirrorsSessionsLayout(t *testing.T) {
	root := t.TempDir()
	c := Corpus{Sessions: 3, MainMeanBytes: 8 << 10}
	if _, err := WritePi(root, c); err != nil {
		t.Fatal(err)
	}
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jsonl" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Errorf("%s: not newline-terminated", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if !piPath.MatchString(rel) {
			t.Errorf("%s: not a Pi session path", rel)
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != c.Sessions {
		t.Fatalf("wrote %d session files, want %d", len(files), c.Sessions)
	}
}

func TestAgentCorpusIngests(t *testing.T) {
	for _, tc := range []struct {
		agent  string
		corpus []File
	}{
		{codex.AgentName, GenerateCodex(Corpus{Sessions: 2, MainMeanBytes: 64 << 10, SubagentsPerSession: 1})},
		{pi.AgentName, GeneratePi(Corpus{Sessions: 2, MainMeanBytes: 64 << 10})},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			ctx := context.Background()
			hub, err := store.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer hub.Close()
			inserted := 0
			var lastSource string
			for j, f := range tc.corpus {
				sourceID := fmt.Sprintf("%s-src-%03d", tc.agent, j)
				lastSource = sourceID
				if err := hub.RegisterSource(ctx, sourceID, "dev-fixgen", tc.agent, "/"+f.Path); err != nil {
					t.Fatal(err)
				}
				res, err := events.Parse(tc.agent, f.Data, 0, events.Options{SourceScope: sourceID})
				if err != nil {
					t.Fatalf("%s: parse: %v", f.Path, err)
				}
				result, err := hub.Ingest(ctx, store.Batch{
					SourceID: sourceID, DeviceID: "dev-fixgen", Events: res.Events})
				if err != nil {
					t.Fatalf("%s: ingest: %v", f.Path, err)
				}
				inserted += result.Inserted
			}
			if inserted == 0 {
				t.Fatal("corpus inserted no events")
			}
			f := tc.corpus[len(tc.corpus)-1]
			sourceID := lastSource
			res, err := events.Parse(tc.agent, f.Data, 0, events.Options{SourceScope: sourceID})
			if err != nil {
				t.Fatal(err)
			}
			result, err := hub.Ingest(ctx, store.Batch{
				SourceID: sourceID, DeviceID: "dev-fixgen", Events: res.Events})
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if result.Inserted != 0 {
				t.Fatalf("replay inserted %d events, want 0", result.Inserted)
			}
		})
	}
}
