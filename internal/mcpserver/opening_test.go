package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
		return &fakeBackend{}, nil, nil
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

func TestOpeningCloseCancelsBlockedOpen(t *testing.T) {
	started := make(chan struct{})
	opening := OpenInBackground(context.Background(), EngineOptions{}, func(ctx context.Context) (Backend, func(), error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	<-started

	done := make(chan error, 1)
	go func() {
		done <- opening.Close()
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close returned %v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close hung while open waited on its context")
	}
}

func TestOpeningDoubleCloseInvokesCloseOnce(t *testing.T) {
	var closed atomic.Int32
	opening := OpenInBackground(context.Background(), EngineOptions{}, func(context.Context) (Backend, func(), error) {
		return &fakeBackend{}, func() { closed.Add(1) }, nil
	})
	if err := opening.Wait(); err != nil {
		t.Fatalf("Wait returned %v; want nil", err)
	}

	if err := opening.Close(); err != nil {
		t.Fatalf("first Close returned %v; want nil", err)
	}
	if err := opening.Close(); err != nil {
		t.Fatalf("second Close returned %v; want nil", err)
	}
	if got := closed.Load(); got != 1 {
		t.Fatalf("closeFn invoked %d times; want 1", got)
	}
}

func TestOpeningRejectsCallsAfterClose(t *testing.T) {
	var backend fakeBackend
	opening := OpenInBackground(context.Background(), EngineOptions{}, func(context.Context) (Backend, func(), error) {
		return &backend, func() {}, nil
	})
	if err := opening.Wait(); err != nil {
		t.Fatalf("Wait returned %v; want nil", err)
	}
	if err := opening.Close(); err != nil {
		t.Fatalf("Close returned %v; want nil", err)
	}

	if _, err := opening.Query(context.Background(), "SELECT 1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Query after Close returned %v; want ErrClosed", err)
	}
	if _, err := opening.AnnotateAs(context.Background(), "dev1", json.RawMessage(`[]`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("AnnotateAs after Close returned %v; want ErrClosed", err)
	}
	if len(backend.queries) != 0 || backend.annotate != 0 {
		t.Fatalf("backend saw calls after Close: %+v", backend)
	}
}

func TestOpeningCloseCleansUpConcurrentBackend(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var closed atomic.Int32

	opening := OpenInBackground(context.Background(), EngineOptions{Deadline: time.Minute}, func(ctx context.Context) (Backend, func(), error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return &fakeBackend{}, func() { closed.Add(1) }, nil
	})
	<-started

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- opening.Close()
	}()
	<-canceled

	if _, err := opening.Query(context.Background(), "SELECT 1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Query while Close awaits open returned %v; want ErrClosed", err)
	}

	close(release)
	if err := <-closeErr; err != nil {
		t.Fatalf("Close returned %v; want nil", err)
	}
	if got := closed.Load(); got != 1 {
		t.Fatalf("closeFn invoked %d times; want 1 before Close returned", got)
	}
	if _, err := opening.AnnotateAs(context.Background(), "dev1", json.RawMessage(`[]`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("AnnotateAs after concurrent Close returned %v; want ErrClosed", err)
	}
}

func TestOpeningNilBackendReturnsErrorWithoutPanicking(t *testing.T) {
	opening := OpenInBackground(context.Background(), EngineOptions{}, func(context.Context) (Backend, func(), error) {
		return nil, nil, nil
	})
	if err := opening.Wait(); !errors.Is(err, errNilBackend) {
		t.Fatalf("Wait returned %v; want a missing backend error", err)
	}
	if _, err := opening.Query(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("Query returned nil error for nil backend; want non-nil error")
	}
	if _, err := opening.AnnotateAs(context.Background(), "dev1", json.RawMessage(`[]`)); err == nil {
		t.Fatal("AnnotateAs returned nil error for nil backend; want non-nil error")
	}
	if err := opening.Close(); !errors.Is(err, errNilBackend) {
		t.Fatalf("Close returned %v; want a missing backend error", err)
	}
}
