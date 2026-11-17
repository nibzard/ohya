package recalleval

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
)

const fixtureDir = "../../testdata/recall"

func buildHub(t *testing.T) (*store.DB, *mcpserver.Engine) {
	t.Helper()
	ctx := context.Background()
	hub, err := store.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	if err := Ingest(ctx, hub, fixtureDir); err != nil {
		t.Fatal(err)
	}
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	return hub, mcpserver.NewEngine(readers, hub.Writer(),
		mcpserver.EngineOptions{WriterID: DeviceID})
}

func TestRecallSetHitsEveryKnownAnswer(t *testing.T) {
	_, engine := buildHub(t)
	report, err := Run(context.Background(), engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if !res.Hit {
			t.Errorf("question %s missed: answer event %d not in top %d for MATCH %q",
				res.Question.Key, res.AnswerID, TopK, res.Question.Match)
		}
	}
	if report.Hits != len(Questions) {
		t.Fatalf("recall %d/%d, want %d/%d", report.Hits, len(Questions),
			len(Questions), len(Questions))
	}
}

func TestRecallDeterministic(t *testing.T) {
	ctx := context.Background()
	_, engineA := buildHub(t)
	reportA, err := Run(ctx, engineA)
	if err != nil {
		t.Fatal(err)
	}
	_, engineB := buildHub(t)
	reportB, err := Run(ctx, engineB)
	if err != nil {
		t.Fatal(err)
	}
	if len(reportA.Results) != len(reportB.Results) {
		t.Fatalf("report lengths %d and %d differ", len(reportA.Results), len(reportB.Results))
	}
	for i, a := range reportA.Results {
		b := reportB.Results[i]
		if a.Question.Key != b.Question.Key {
			t.Fatalf("result %d keys %s and %s differ", i, a.Question.Key, b.Question.Key)
		}
		if a.AnswerID != b.AnswerID {
			t.Errorf("question %s resolved to event %d and %d",
				a.Question.Key, a.AnswerID, b.AnswerID)
		}
		if a.Rank != b.Rank {
			t.Errorf("question %s ranked %d and %d", a.Question.Key, a.Rank, b.Rank)
		}
	}
}

func TestRecallMeasuresUnindexedToolOutputs(t *testing.T) {
	ctx := context.Background()
	hub, engine := buildHub(t)
	before, err := Run(ctx, engine)
	if err != nil {
		t.Fatal(err)
	}
	if before.Misses != 0 {
		t.Fatalf("baseline recall %d/%d, want %d/%d", before.Hits, len(Questions),
			len(Questions), len(Questions))
	}
	const dropToolOutputs = `INSERT INTO events_fts(events_fts, rowid, text)
SELECT 'delete', id, text FROM events
WHERE (kind = 'response_item' AND tool_name IS NULL
         AND json_extract(metadata, '$.callId') IS NOT NULL)
   OR (kind = 'user' AND tool_name IS NULL
         AND json_extract(metadata, '$.blocks[0].type') = 'tool_result')`
	if _, err := hub.Writer().ExecContext(ctx, dropToolOutputs); err != nil {
		t.Fatal(err)
	}
	after, err := Run(ctx, engine)
	if err != nil {
		t.Fatal(err)
	}
	toolOutputAnswers := map[string]bool{
		"registry-locked":       true,
		"server-repo-locked":    true,
		"uploader-test-failure": true,
	}
	for _, res := range after.Results {
		if toolOutputAnswers[res.Question.Key] && res.Hit {
			t.Errorf("question %s still hit after its tool-output answer left the index",
				res.Question.Key)
		}
		if !toolOutputAnswers[res.Question.Key] && !res.Hit {
			t.Errorf("question %s missed, want hit", res.Question.Key)
		}
	}
	wantHits := len(Questions) - len(toolOutputAnswers)
	if after.Misses != len(toolOutputAnswers) || after.Hits != wantHits {
		t.Fatalf("recall after cut %d/%d, want %d/%d", after.Hits, len(Questions),
			wantHits, len(Questions))
	}
}
