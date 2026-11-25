package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nibzard/ohya/internal/store"
)

type fakeBackend struct {
	queries  []string
	annotate int
}

func (f *fakeBackend) Query(ctx context.Context, query string) (Result, error) {
	f.queries = append(f.queries, query)
	return Result{Columns: []string{"ok"}, Rows: [][]any{{1}}, RowCount: 1}, nil
}

func (f *fakeBackend) AnnotateAs(ctx context.Context, writerID string, raw json.RawMessage) (store.AnnotateResult, error) {
	f.annotate++
	return store.AnnotateResult{Applied: 1}, nil
}

func TestOpeningToolCallsWaitForTheDeadlineThenReportTheOpen(t *testing.T) {
	var backend fakeBackend
	closed := 0
	release := make(chan struct{})
	var releaseOnce sync.Once
	opening := OpenInBackground(context.Background(), EngineOptions{Deadline: 20 * time.Millisecond}, func(context.Context) (Backend, func(), error) {
		<-release
		return &backend, func() { closed++ }, nil
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		opening.Close()
	})

	started := time.Now()
	_, err := opening.Query(context.Background(), "SELECT 1")
	if !errors.Is(err, ErrOpening) || time.Since(started) < 20*time.Millisecond {
		t.Fatalf("Query returned %v after %v; want ErrOpening after the deadline", err, time.Since(started))
	}
	if _, err := opening.AnnotateAs(context.Background(), "dev1", json.RawMessage(`[]`)); !errors.Is(err, ErrOpening) {
		t.Fatalf("AnnotateAs returned %v while the database opens; want ErrOpening", err)
	}
	if len(backend.queries) != 0 || backend.annotate != 0 {
		t.Fatalf("calls reached a backend that is not open: %+v", backend)
	}

	releaseOnce.Do(func() { close(release) })
	if err := opening.Wait(); err != nil {
		t.Fatalf("Wait returned %v; want nil", err)
	}
	result, err := opening.Query(context.Background(), "SELECT 2")
	if err != nil || result.RowCount != 1 {
		t.Fatalf("Query after the open returned %+v, %v; want the backend result", result, err)
	}
	if _, err := opening.AnnotateAs(context.Background(), "dev1", json.RawMessage(`[]`)); err != nil {
		t.Fatalf("AnnotateAs after the open returned %v", err)
	}
	if len(backend.queries) != 1 || backend.queries[0] != "SELECT 2" || backend.annotate != 1 {
		t.Fatalf("backend saw %+v; want one query and one annotate", backend)
	}
	if err := opening.Close(); err != nil || closed != 1 {
		t.Fatalf("Close returned %v and closed the backend %d times; want nil and 1", err, closed)
	}
}

func TestOpeningReturnsTheOpenError(t *testing.T) {
	want := errors.New("disk I/O error")
	closed := false
	opening := OpenInBackground(context.Background(), EngineOptions{}, func(context.Context) (Backend, func(), error) {
		return nil, nil, want
	})
	if err := opening.Wait(); !errors.Is(err, want) {
		t.Fatalf("Wait returned %v; want the open error", err)
	}
	if _, err := opening.Query(context.Background(), "SELECT 1"); !errors.Is(err, want) {
		t.Fatalf("Query returned %v; want the open error", err)
	}
	if _, err := opening.AnnotateAs(context.Background(), "dev1", nil); !errors.Is(err, want) {
		t.Fatalf("AnnotateAs returned %v; want the open error", err)
	}
	if err := opening.Close(); !errors.Is(err, want) || closed {
		t.Fatalf("Close returned %v with the backend closed = %v; want the open error and no backend", err, closed)
	}
}

func TestOpeningCallEndsWithItsOwnContext(t *testing.T) {
	release := make(chan struct{})
	opening := OpenInBackground(context.Background(), EngineOptions{Deadline: time.Minute}, func(context.Context) (Backend, func(), error) {
		<-release
		return nil, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := opening.Query(ctx, "SELECT 1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Query returned %v; want the deadline of its own context", err)
	}
	close(release)
	if err := opening.Close(); err != nil {
		t.Fatalf("Close returned %v; want nil", err)
	}
}

func TestOpeningEndsWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opening := OpenInBackground(ctx, EngineOptions{}, func(ctx context.Context) (Backend, func(), error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	cancel()
	if err := opening.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait returned %v; want context.Canceled", err)
	}
}

func TestServerAnswersWhileTheDatabaseOpens(t *testing.T) {
	o := EngineOptions{Deadline: 50 * time.Millisecond}
	hub, readers := openSeededHub(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	opening := OpenInBackground(context.Background(), o, func(context.Context) (Backend, func(), error) {
		<-release
		return NewEngine(readers, hub.Writer(), o), func() {}, nil
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		opening.Close()
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(opening, o).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("tool list while the database opens = %+v, %v; want sql and annotate", tools, err)
	}
	for name, arguments := range map[string]map[string]any{
		ToolName:         {"query": "SELECT 1"},
		AnnotateToolName: {"operations": json.RawMessage(`[]`)},
	} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
		if err != nil {
			t.Fatalf("CallTool(%s) while the database opens: %v", name, err)
		}
		text, _ := result.Content[0].(*mcp.TextContent)
		if !result.IsError || !strings.Contains(text.Text, "still opening") {
			t.Fatalf("%s while the database opens = %+v; want an error that says the database is still opening", name, result)
		}
	}

	releaseOnce.Do(func() { close(release) })
	if err := opening.Wait(); err != nil {
		t.Fatalf("Wait returned %v; want nil", err)
	}
	result := call(t, session, `SELECT count(*) AS events FROM events`)
	if result.IsError {
		t.Fatalf("select after the open failed: %+v", result)
	}
	var payload Result
	if err := redecode(result, &payload); err != nil || payload.RowCount != 1 || fmt.Sprint(payload.Rows) != "[[2]]" {
		t.Fatalf("payload after the open = %+v, %v; want the seeded rows", payload, err)
	}
}
